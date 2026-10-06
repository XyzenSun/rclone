// Package wopan provides an interface to WoPan (联通云盘, pan.wo.cn), the
// cloud storage service of China Unicom.
//
// The protocol implementation (signing, encryption, upload orchestration) is
// provided by the wopan-sdk-go SDK. This package adapts it to the rclone fs
// interface, following the driver behaviour verified from OpenList.
package wopan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	wopansdk "github.com/OpenListTeam/wopan-sdk-go"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/random"
)

const (
	minSleep = 200 * time.Millisecond
	maxSleep = 2 * time.Second
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "wopan",
		Description: "WoPan (China Unicom Cloud Disk)",
		NewFs:       NewFs,
		Config:      Config,
		Options: []fs.Option{{
			Name: "family_id",
			Help: `Family Cloud ID.

Leave empty to use your personal cloud. Set this to switch to the
family cloud of the given ID.`,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name: "root_folder_id",
			Help: `ID of the root folder.

Leave blank normally. Fill in to use a non-root folder as the starting point.
Defaults to "0", the root folder of WoPan.`,
			Default:   wopanRootFolderID,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:      "refresh_token",
			Help:      "OAuth refresh token - set by the config wizard.",
			Hide:      fs.OptionHideBoth,
			Sensitive: true,
		}, {
			Name:      "access_token",
			Help:      "OAuth access token - set by the config wizard, may be empty.",
			Hide:      fs.OptionHideBoth,
			Sensitive: true,
		}, {
			Name:     "encoding",
			Help:     "The encoding for the backend.\n\nSee the [encoding section in the overview](/overview/#encoding) for more information.",
			Advanced: true,
			Default: (encoder.EncodeInvalidUtf8 |
				encoder.EncodeCtl |
				encoder.EncodeDel |
				encoder.EncodeDot |
				encoder.EncodeQuestion |
				encoder.EncodeAsterisk |
				encoder.EncodeLtGt |
				encoder.EncodeSlash),
		}},
	})
}

// Options defines the configuration for this backend
type Options struct {
	FamilyID     string               `config:"family_id"`
	RootFolderID string               `config:"root_folder_id"`
	RefreshToken string               `config:"refresh_token"`
	AccessToken  string               `config:"access_token"`
	Enc          encoder.MultiEncoder `config:"encoding"`
}

// Fs stores the interface to the WoPan files
type Fs struct {
	name            string
	root            string
	opt             Options
	features        *fs.Features
	client          *wopansdk.WoClient
	httpClient      *http.Client // used for downloading via direct links
	dirCache        *dircache.DirCache
	pacer           *fs.Pacer
	defaultFamilyID string // personal cloud CreateDirectory requires this as familyId
}

// Object is a remote object that has been stat'd
type Object struct {
	fs      *Fs
	remote  string
	size    int64
	modTime time.Time
	id      string // API id, used by move/copy/remove/rename
	fid     string // fid, used only by GetDownloadUrlV2
}

// Config is the state machine for `rclone config`, offering two ways to
// authenticate: interactive login (phone + password + SMS code) or pasting a
// refresh_token captured by other means.
func Config(ctx context.Context, name string, m configmap.Mapper, conf fs.ConfigIn) (*fs.ConfigOut, error) {
	switch conf.State {
	case "":
		return fs.ConfigChooseExclusiveFixed("auth_method", "config_auth_method", "Select how to authenticate", []fs.OptionExample{{
			Value: "login",
			Help:  "Log in with phone number, password and SMS verification code",
		}, {
			Value: "token",
			Help:  "Paste a refresh_token captured from the web page (access_token is optional)",
		}})
	case "auth_method":
		switch conf.Result {
		case "login":
			return fs.ConfigInput("login_phone", "config_phone", "Phone number of your WoPan account")
		case "token":
			return fs.ConfigPassword("token_refresh", "config_refresh_token", "Paste your refresh_token")
		default:
			return nil, fmt.Errorf("unknown authentication method %q", conf.Result)
		}
	case "login_phone":
		m.Set("config_phone", conf.Result)
		return fs.ConfigPassword("login_password", "config_password", "Password of your account.\n\nOnly used once to log in, it will not be stored.")
	case "login_password":
		m.Set("config_password", conf.Result)
		phone, _ := m.Get("config_phone")
		w := wopansdk.Default()
		w.SetHttpClient(fshttp.NewClient(ctx))
		// PcWebLogin triggers the provider to send the SMS verification code
		if _, err := w.PcWebLogin(phone, obscure.MustReveal(conf.Result)); err != nil {
			return nil, fmt.Errorf("login failed: %w", err)
		}
		return fs.ConfigInput("login_sms", "config_sms_code", "SMS verification code sent to your phone")
	case "login_sms":
		phone, _ := m.Get("config_phone")
		password, _ := m.Get("config_password")
		w := wopansdk.Default()
		w.SetHttpClient(fshttp.NewClient(ctx))
		data, err := w.PcLoginVerifyCode(phone, obscure.MustReveal(password), conf.Result)
		if err != nil {
			return nil, fmt.Errorf("SMS verification failed: %w", err)
		}
		m.Set("refresh_token", data.RefreshToken)
		m.Set("access_token", data.AccessToken)
		return nil, nil
	case "token_refresh":
		// ConfigPassword 的输入会被 obscure 加密，存储前需要还原
		m.Set("refresh_token", obscure.MustReveal(conf.Result))
		return fs.ConfigInputOptional("token_access", "config_access_token", "Optionally paste your access_token.\n\nLeave empty to fetch a new one with the refresh_token on first use.")
	case "token_access":
		if conf.Result != "" {
			m.Set("access_token", obscure.MustReveal(conf.Result))
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown state %q", conf.State)
}

// NewFs creates a new Fs object from the name and root
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	root = strings.Trim(root, "/")

	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}
	if opt.RootFolderID == "" {
		opt.RootFolderID = wopanRootFolderID
	}
	if opt.RefreshToken == "" {
		return nil, errors.New("refresh_token is required - run `rclone config` to set up this remote")
	}

	f := &Fs{
		name:   name,
		root:   root,
		opt:    *opt,
		pacer:  fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep))),
		client: wopansdk.DefaultWithRefreshToken(opt.RefreshToken),
	}
	f.httpClient = fshttp.NewClient(ctx)
	f.client.SetHttpClient(f.httpClient)
	f.client.SetAccessToken(opt.AccessToken)
	// SDK 在业务响应码异常时会自动刷新 token 并重试一次，回调负责把新
	// token 回写配置文件，避免重启后使用过期 token
	f.client.OnRefreshToken(func(accessToken, refreshToken string) {
		f.opt.AccessToken = accessToken
		f.opt.RefreshToken = refreshToken
		if err := fs.ConfigFileSet(name, "access_token", accessToken); err != nil {
			fs.Logf(f, "Failed to save access_token in config file: %v", err)
		}
		if err := fs.ConfigFileSet(name, "refresh_token", refreshToken); err != nil {
			fs.Logf(f, "Failed to save refresh_token in config file: %v", err)
		}
	})
	f.dirCache = dircache.New(root, opt.RootFolderID, f)

	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	// 初始化顺序照搬 OpenList 驱动：先确保有有效 access_token（空时先刷新），
	// 再取家庭信息（个人云建目录时需要 defaultFamilyId 作为 familyId），
	// 最后初始化 SDK 的 zone URL 与分类规则
	if f.opt.AccessToken == "" {
		err = f.pacer.Call(func() (bool, error) {
			err := f.client.RefreshToken()
			return shouldRetry(err), err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to refresh token: %w", err)
		}
	}
	err = f.pacer.Call(func() (bool, error) {
		data, err := f.client.FamilyUserCurrentEncode()
		if err != nil {
			return shouldRetry(err), err
		}
		f.defaultFamilyID = strconv.Itoa(data.DefaultHomeId)
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get family info: %w", err)
	}
	err = f.pacer.Call(func() (bool, error) {
		err := f.client.InitData()
		return shouldRetry(err), err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialise client: %w", err)
	}

	// Find the current root
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f //nolint:govet // copying mutex is OK here as it is a new Fs
		tempF.dirCache = dircache.New(newRoot, opt.RootFolderID, &tempF)
		tempF.root = newRoot
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				// File doesn't exist so return old f
				return f, nil
			}
			return nil, err
		}
		f.features.Fill(ctx, &tempF)
		// XXX: update the old f here instead of returning tempF, since
		// `features` were already filled with functions having *f as the receiver.
		// See https://github.com/rclone/rclone/issues/2182
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}
	return f, nil
}

// fileToObject converts an SDK File into an Object, parsing the server
// createTime as the read-only modification time
func (f *Fs) fileToObject(remote string, file *wopansdk.File) (o *Object, err error) {
	modTime, err := parseCreateTime(file.CreateTime)
	if err != nil {
		return nil, fmt.Errorf("failed to parse createTime %q of %q: %w", file.CreateTime, remote, err)
	}
	return &Object{
		fs:      f,
		remote:  remote,
		size:    file.Size,
		modTime: modTime,
		id:      file.Id,
		fid:     file.Fid,
	}, nil
}

// listAll lists the directory dirID, calling fn for each entry.
// fn 返回 true 时提前终止遍历并返回 found=true。
func (f *Fs) listAll(ctx context.Context, dirID string, fn func(*wopansdk.File) bool) (found bool, err error) {
	pageNum := 0
	for {
		var data *wopansdk.QueryAllFilesData
		err = f.pacer.Call(func() (bool, error) {
			data, err = f.client.QueryAllFiles(spaceTypeOf(f.opt.FamilyID), dirID, pageNum, wopanPageSize, wopansdk.SortNameAsc, f.opt.FamilyID)
			return shouldRetry(err), err
		})
		if err != nil {
			if isNotFoundErr(err) {
				return false, fs.ErrorDirNotFound
			}
			return false, fmt.Errorf("failed to list directory %q: %w", dirID, err)
		}
		// 在列表边界把存储侧名字转换为 rclone 标准形式，下游的比较与
		// remote 拼接全部基于标准名字
		for i := range data.Files {
			data.Files[i].Name = f.opt.Enc.ToStandardName(data.Files[i].Name)
		}
		for i := range data.Files {
			if fn(&data.Files[i]) {
				return true, nil
			}
		}
		if !hasMorePage(len(data.Files), wopanPageSize) {
			break
		}
		pageNum++
	}
	return false, nil
}

// FindLeaf finds a directory of name leaf in the folder with ID directoryID,
// required by dircache.DirCacher
func (f *Fs) FindLeaf(ctx context.Context, directoryID, leaf string) (directoryIDOut string, found bool, err error) {
	found, err = f.listAll(ctx, directoryID, func(file *wopansdk.File) bool {
		if file.Type == 0 && file.Name == leaf {
			directoryIDOut = file.Id
			return true
		}
		return false
	})
	return directoryIDOut, found, err
}

// familyIDForCreate 返回建目录 API 需要的 familyId 参数：个人云时必须传
// defaultFamilyId（FamilyUserCurrentEncode 的 DefaultHomeId），这是联通
// API 的特有要求，照搬 OpenList 驱动行为
func (f *Fs) familyIDForCreate() string {
	if spaceTypeOf(f.opt.FamilyID) == wopansdk.SpaceTypePersonal {
		return f.defaultFamilyID
	}
	return f.opt.FamilyID
}

// CreateDir makes a directory with dirID as parent and name leaf, required by
// dircache.DirCacher
func (f *Fs) CreateDir(ctx context.Context, dirID, leaf string) (newID string, err error) {
	var data *wopansdk.CreateDirectoryData
	err = f.pacer.Call(func() (bool, error) {
		data, err = f.client.CreateDirectory(spaceTypeOf(f.opt.FamilyID), dirID, f.opt.Enc.FromStandardName(leaf), f.familyIDForCreate())
		return shouldRetry(err), err
	})
	if err != nil {
		return "", fmt.Errorf("failed to create directory %q: %w", leaf, err)
	}
	if data.Id == "" {
		return "", errors.New("wopan returned an empty ID for the newly created directory")
	}
	return data.Id, nil
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	_, err = f.listAll(ctx, directoryID, func(file *wopansdk.File) bool {
		remote := path.Join(dir, file.Name)
		modTime, err := parseCreateTime(file.CreateTime)
		if err != nil {
			fs.Logf(f, "Failed to parse createTime %q of %q: %v", file.CreateTime, remote, err)
			modTime = time.Time{}
		}
		if file.Type == 0 {
			// 目录：登记进 dirCache，加速后续路径解析
			f.dirCache.Put(remote, file.Id)
			entries = append(entries, fs.NewDir(remote, modTime).SetID(file.Id))
			return false
		}
		o, err := f.fileToObject(remote, file)
		if err != nil {
			fs.Logf(f, "Skipping entry %q: %v", remote, err)
			return false
		}
		entries = append(entries, o)
		return false
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// findObjectInDir locates a file named leaf inside directory dirID
func (f *Fs) findObjectInDir(ctx context.Context, dirID, leaf string) (file *wopansdk.File, found bool, err error) {
	found, err = f.listAll(ctx, dirID, func(fi *wopansdk.File) bool {
		if fi.Type != 0 && fi.Name == leaf {
			file = fi
			return true
		}
		return false
	})
	return file, found, err
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	file, found, err := f.findObjectInDir(ctx, dirID, leaf)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fs.ErrorObjectNotFound
	}
	return f.fileToObject(remote, file)
}

// Mkdir creates the container if it doesn't exist
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// deleteFiles deletes directories and/or files by ID using one API call
func (f *Fs) deleteFiles(ctx context.Context, dirList, fileList []string) (err error) {
	if len(dirList) == 0 && len(fileList) == 0 {
		return nil
	}
	// 联通 API 要求列表参数序列化为空数组而非 null，nil 必须归一化，
	// 否则服务端报 9999 系统异常
	if dirList == nil {
		dirList = []string{}
	}
	if fileList == nil {
		fileList = []string{}
	}
	err = f.pacer.Call(func() (bool, error) {
		err := f.client.DeleteFile(spaceTypeOf(f.opt.FamilyID), dirList, fileList)
		return shouldRetry(err), err
	})
	return err
}

// Rmdir removes the directory if it is empty
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	// rclone 语义：Rmdir 只允许删空目录；联通的 DeleteFile 会删整棵子树，
	// 必须先检查目录是否为空
	found, err := f.listAll(ctx, dirID, func(file *wopansdk.File) bool {
		return true // 任意条目即非空
	})
	if err != nil {
		return err
	}
	if found {
		return fs.ErrorDirectoryNotEmpty
	}
	if err := f.deleteFiles(ctx, []string{dirID}, nil); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// purgeCheck deletes the directory and all of its contents
func (f *Fs) purgeCheck(ctx context.Context, dir string) error {
	// 仅在 Fs 直接指向真实根目录时拒绝（删除根目录会清空整个云盘）；
	// rclone purge 子目录时 Fs root 就是该子目录，dir 为空是正常情况
	if f.root == "" && dir == "" {
		return errors.New("can't purge root directory")
	}
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	// DeleteFile 对目录按整棵子树删除（待验收确认），因此只需删除顶层目录 ID
	if err := f.deleteFiles(ctx, []string{dirID}, nil); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// Purge deletes all the files and the container
func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir)
}

// CleanUp empties the recycle bin
func (f *Fs) CleanUp(ctx context.Context) (err error) {
	err = f.pacer.Call(func() (bool, error) {
		err := f.client.EmptyRecycleData()
		return shouldRetry(err), err
	})
	if err != nil {
		return fmt.Errorf("failed to empty recycle bin: %w", err)
	}
	return nil
}

// About gets quota information
func (f *Fs) About(ctx context.Context) (usage *fs.Usage, err error) {
	var info *wopansdk.QueryCloudUsageInfoData
	err = f.pacer.Call(func() (bool, error) {
		info, err = f.client.QueryCloudUsageInfo()
		return shouldRetry(err), err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get cloud usage info: %w", err)
	}
	usage = &fs.Usage{
		Used: fs.NewUsageValue(info.UsageInfo.ByteUsedSize),
	}
	// ByteTotalSize 在 API 中是字符串，需要解析
	if total, parseErr := strconv.ParseInt(info.UsageInfo.ByteTotalSize, 10, 64); parseErr == nil && total > 0 {
		usage.Total = fs.NewUsageValue(total)
		usage.Free = fs.NewUsageValue(total - info.UsageInfo.ByteUsedSize)
	}
	return usage, nil
}

// getDownloadURL fetches a fresh direct download link for the object's fid
func (o *Object) getDownloadURL(ctx context.Context) (url string, err error) {
	var data *wopansdk.GetDownloadUrlV2Data
	err = o.fs.pacer.Call(func() (bool, error) {
		data, err = o.fs.client.GetDownloadUrlV2([]string{o.fid})
		return shouldRetry(err), err
	})
	if err != nil {
		return "", fmt.Errorf("failed to get download URL: %w", err)
	}
	if len(data.List) == 0 || data.List[0].DownloadUrl == "" {
		return "", errors.New("download URL is empty")
	}
	return data.List[0].DownloadUrl, nil
}

// Open an object for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (in io.ReadCloser, err error) {
	if o.fid == "" {
		return nil, errors.New("can't download: no fid")
	}
	var res *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		url, err := o.getDownloadURL(ctx)
		if err != nil {
			return shouldRetry(err), err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, err
		}
		fs.FixRangeOption(options, o.size)
		fs.OpenOptionAddHTTPHeaders(req.Header, options)
		if o.size == 0 {
			// 0 长度对象发 Range 请求必然失败，直接删除 Range 头
			req.Header.Del("Range")
		}
		res, err = o.fs.httpClient.Do(req)
		if err != nil {
			return shouldRetry(err), err
		}
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
			statusCode := res.StatusCode
			_ = res.Body.Close()
			res = nil
			// 直链有效期较短，403/410 视为过期，让 pacer 重试时重新取链接
			return statusCode == http.StatusForbidden || statusCode == http.StatusGone ||
					statusCode == http.StatusTooManyRequests || statusCode >= 500,
				fmt.Errorf("download failed: HTTP status %d", statusCode)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Update the object with the contents of the io.Reader
//
// wopan 不支持覆盖上传，检测到同名旧文件时先删除再上传。
// 上传走 SDK 的 Upload2C：内部按 8MB 顺序分片，从 in 顺序读取。
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	leaf, dirID, err := o.fs.dirCache.FindPath(ctx, o.Remote(), true)
	if err != nil {
		return err
	}
	size := src.Size()
	if size == 0 {
		// 联通的 upload2C 接口不接受 0 字节分片（服务端返回 400），
		// 已实测确认无变体可用，这是服务端硬限制
		return errors.New("wopan does not support uploading zero-length files")
	}
	if size < 0 {
		// 未知大小（如 rcat 流式输入）：SDK 需要预先知道大小来计算分片数，
		// 只能缓冲到内存后上传
		fs.Debugf(o, "Unknown size upload - buffering in memory")
		buffered, err := io.ReadAll(in)
		if err != nil {
			return fmt.Errorf("failed to buffer unknown-size upload: %w", err)
		}
		in = bytes.NewReader(buffered)
		size = int64(len(buffered))
		if size == 0 {
			return errors.New("wopan does not support uploading zero-length files")
		}
	}

	// 覆盖语义：目标已存在则先删除旧文件，上传中途失败不会留下半成品
	// （旧文件已删，由 rclone 上层重试整个上传）
	if oldFile, found, findErr := o.fs.findObjectInDir(ctx, dirID, leaf); findErr == nil && found {
		if err := o.fs.deleteFiles(ctx, nil, []string{oldFile.Id}); err != nil {
			return fmt.Errorf("failed to remove existing file before upload: %w", err)
		}
	} else if findErr != nil && findErr != fs.ErrorDirNotFound {
		return findErr
	}

	fid, err := o.fs.client.Upload2C(spaceTypeOf(o.fs.opt.FamilyID), wopansdk.Upload2CFile{
		Name:        o.fs.opt.Enc.FromStandardName(leaf),
		Size:        size,
		Content:     in,
		ContentType: fs.MimeType(ctx, src),
	}, dirID, o.fs.opt.FamilyID, wopansdk.Upload2COption{Ctx: ctx})
	if err != nil {
		// 上传无法在 backend 内重试（流不可重读），但瞬时的 5xx/限流
		// 应标记为可重试，让 rclone 核心层重新打开源后重传
		if shouldRetry(err) {
			err = fserrors.RetryError(err)
		}
		return fmt.Errorf("failed to upload %q: %w", leaf, err)
	}

	// 使父目录缓存失效，随后按名字回读新文件的元数据；
	// 回读失败时用已知信息构造 Object（fid 至少保证可下载）
	dir, _ := dircache.SplitPath(o.Remote())
	o.fs.dirCache.FlushDir(dir)
	o.size = size
	o.modTime = src.ModTime(ctx)
	o.fid = fid
	o.id = ""
	if file, found, findErr := o.fs.findObjectInDir(ctx, dirID, leaf); findErr == nil && found {
		if newObj, convErr := o.fs.fileToObject(o.Remote(), file); convErr == nil {
			*o = *newObj
		}
	}
	return nil
}

// Put the object
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existing, err := f.NewObject(ctx, src.Remote())
	switch {
	case err == nil:
		// 目标已存在，走 Update 的覆盖语义
		return existing, existing.Update(ctx, in, src, options...)
	case errors.Is(err, fs.ErrorObjectNotFound):
		// fall through, create a new object below
	default:
		return nil, err
	}
	o := &Object{
		fs:     f,
		remote: src.Remote(),
	}
	if err := o.Update(ctx, in, src, options...); err != nil {
		return nil, err
	}
	return o, nil
}

// sleepContext 等待指定时长，可被 ctx 取消中断
func sleepContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// moveOrCopy moves or copies entries into targetDirID, both staying inside
// the configured space (cross-family operations are not supported by the API)
func (f *Fs) moveOrCopy(move bool, dirList, fileList []string, targetDirID string) (err error) {
	// 联通 API 要求列表参数序列化为空数组而非 null，nil 必须归一化，
	// 否则服务端报 9999 系统异常
	if dirList == nil {
		dirList = []string{}
	}
	if fileList == nil {
		fileList = []string{}
	}
	err = f.pacer.Call(func() (bool, error) {
		if move {
			err = f.client.MoveFile(dirList, fileList, targetDirID,
				spaceTypeOf(f.opt.FamilyID), spaceTypeOf(f.opt.FamilyID), f.opt.FamilyID, f.opt.FamilyID)
		} else {
			err = f.client.CopyFile(dirList, fileList, targetDirID,
				spaceTypeOf(f.opt.FamilyID), spaceTypeOf(f.opt.FamilyID), f.opt.FamilyID, f.opt.FamilyID)
		}
		return shouldRetry(err), err
	})
	return err
}

// rename renames a file (typ=1) or directory (typ=0) by ID
func (f *Fs) rename(ctx context.Context, typ int, id, newName string) (err error) {
	err = f.pacer.Call(func() (bool, error) {
		err := f.client.RenameFileOrDirectory(spaceTypeOf(f.opt.FamilyID), typ, id, f.opt.Enc.FromStandardName(newName), f.opt.FamilyID)
		return shouldRetry(err), err
	})
	return err
}

// removeConflictingFile deletes an existing file named leaf in dirID so a
// subsequent move/copy/upload into it has overwrite semantics.
// 目标不存在时视为成功。
func (f *Fs) removeConflictingFile(ctx context.Context, dirID, leaf string) error {
	dstFile, found, err := f.findObjectInDir(ctx, dirID, leaf)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return nil
		}
		return err
	}
	if !found {
		return nil
	}
	return f.deleteFiles(ctx, nil, []string{dstFile.Id})
}

// Move src to this remote using server-side move operations
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (dst fs.Object, err error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}
	// 目标目录不存在时自动创建（与 pikpak 等后端行为一致）
	dstLeaf, dstDirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	srcParent, srcLeaf := dircache.SplitPath(srcObj.remote)
	dstParent, _ := dircache.SplitPath(remote)
	// 父目录与同名判断必须用绝对路径：rclone 跨子目录移动时 src/dst
	// 是不同 Fs 实例（root 不同），相对路径不可直接比较
	srcParentAbs := path.Join(srcObj.fs.root, srcParent)
	dstParentAbs := path.Join(f.root, dstParent)
	srcRemoteAbs := path.Join(srcObj.fs.root, srcObj.remote)
	dstRemoteAbs := path.Join(f.root, remote)

	// Move 语义是覆盖：目标位置已有同名文件时先删除
	if srcRemoteAbs != dstRemoteAbs {
		if err := f.removeConflictingFile(ctx, dstDirID, dstLeaf); err != nil {
			return nil, fmt.Errorf("failed to remove existing destination file: %w", err)
		}
	}
	// 服务端移动仅在跨目录时执行；同目录时等价于重命名，避开
	// API 对"移动到当前目录"的限制（与 pikpak 的处理方式一致）
	if srcParentAbs != dstParentAbs {
		if err := f.moveOrCopy(true, nil, []string{srcObj.id}, dstDirID); err != nil {
			return nil, fmt.Errorf("failed to move file: %w", err)
		}
	}
	if srcLeaf != dstLeaf {
		if err := f.rename(ctx, 1, srcObj.id, dstLeaf); err != nil {
			return nil, fmt.Errorf("failed to rename moved file: %w", err)
		}
	}
	// 刷新两侧父目录缓存：src 与 dst 可能属于不同 Fs 实例，
	// 各自的 dirCache 要用各自 root 下的相对路径刷新
	srcObj.fs.dirCache.FlushDir(srcParent)
	if srcObj.fs != f {
		f.dirCache.FlushDir(dstParent)
	}
	// 服务端移动/重命名存在短暂可见性延迟，操作后立即重读元数据可能
	// 拿到旧状态；直接复用已知信息更新对象返回（id 不因移动/重命名变化）
	srcObj.remote = remote
	return srcObj, nil
}

// Copy src to this remote using server-side copy operations
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (dst fs.Object, err error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	// 目标目录不存在时自动创建（与 Move 行为一致）
	dstLeaf, dstDirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	srcParent, srcLeaf := dircache.SplitPath(srcObj.remote)
	dstParent, _ := dircache.SplitPath(remote)
	// 父目录与同名判断必须用绝对路径，理由同 Move
	srcParentAbs := path.Join(srcObj.fs.root, srcParent)
	dstParentAbs := path.Join(f.root, dstParent)
	if srcParentAbs == dstParentAbs && srcLeaf == dstLeaf {
		// 同名同目录复制无意义
		fs.Debugf(src, "Can't copy - same name in same directory")
		return nil, fs.ErrorCantCopy
	}
	if err := f.removeConflictingFile(ctx, dstDirID, dstLeaf); err != nil {
		return nil, fmt.Errorf("failed to remove existing destination file: %w", err)
	}
	if srcParentAbs == dstParentAbs {
		// 同目录复制：CopyFile 保留源文件名，副本与源文件同名冲突时
		// 服务端会自动重命名源文件，必须经临时目录中转
		return f.copySameDir(ctx, srcObj, remote, dstDirID, srcLeaf, dstLeaf)
	}
	if err := f.moveOrCopy(false, nil, []string{srcObj.id}, dstDirID); err != nil {
		return nil, fmt.Errorf("failed to copy file: %w", err)
	}
	f.dirCache.FlushDir(dstParent)
	// 服务端副本存在短暂可见性延迟，重读元数据时重试几次
	var copied fs.Object
	for range 3 {
		copied, err = f.NewObject(ctx, path.Join(dstParent, srcLeaf))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrorObjectNotFound) {
			return nil, fmt.Errorf("failed to locate copied file: %w", err)
		}
		if err := sleepContext(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to locate copied file: %w", err)
	}
	dst = copied
	if srcLeaf != dstLeaf {
		dstObj := dst.(*Object)
		if err := f.rename(ctx, 1, dstObj.id, dstLeaf); err != nil {
			return nil, fmt.Errorf("failed to rename copied file: %w", err)
		}
		dstObj.remote = remote
	}
	return dst, nil
}

// copySameDir 在同目录内以新名字复制文件：副本先落到临时目录并改名，
// 再移回目标目录，避开服务端对同名冲突的自动重命名
func (f *Fs) copySameDir(ctx context.Context, srcObj *Object, remote, dstDirID, srcLeaf, dstLeaf string) (dst fs.Object, err error) {
	tempDirName := "rclone-copy-" + random.String(8)
	var data *wopansdk.CreateDirectoryData
	err = f.pacer.Call(func() (bool, error) {
		data, err = f.client.CreateDirectory(spaceTypeOf(f.opt.FamilyID), dstDirID, f.opt.Enc.FromStandardName(tempDirName), f.familyIDForCreate())
		return shouldRetry(err), err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory for copy: %w", err)
	}
	tempDirID := data.Id
	defer func() {
		if cleanErr := f.deleteFiles(ctx, []string{tempDirID}, nil); cleanErr != nil {
			fs.Logf(f, "Failed to remove temp directory %q: %v", tempDirName, cleanErr)
		}
	}()

	// 副本复制到临时目录（空目录，不会冲突）
	if err := f.moveOrCopy(false, nil, []string{srcObj.id}, tempDirID); err != nil {
		return nil, fmt.Errorf("failed to copy file: %w", err)
	}
	// 在临时目录里定位副本（服务端副本有短暂可见性延迟）
	var copyFile *wopansdk.File
	for range 3 {
		var found bool
		copyFile, found, err = f.findObjectInDir(ctx, tempDirID, srcLeaf)
		if err == nil && found {
			break
		}
		copyFile = nil
		if err != nil {
			return nil, fmt.Errorf("failed to locate copied file: %w", err)
		}
		if err := sleepContext(ctx, 500*time.Millisecond); err != nil {
			return nil, err
		}
	}
	if copyFile == nil {
		return nil, errors.New("failed to locate copied file in temp directory")
	}
	// 副本在临时目录里改名为目标名，避开目标目录中的同名冲突
	if err := f.rename(ctx, 1, copyFile.Id, dstLeaf); err != nil {
		return nil, fmt.Errorf("failed to rename copied file: %w", err)
	}
	// 改名后的副本移回目标目录
	if err := f.moveOrCopy(true, nil, []string{copyFile.Id}, dstDirID); err != nil {
		return nil, fmt.Errorf("failed to move copied file: %w", err)
	}
	// 复用已知的副本元数据构造返回对象（id/fid 不因移动/重命名变化）
	return f.fileToObject(remote, copyFile)
}

// DirMove moves src, srcRemote to this remote at dstRemote using server-side
// move operations
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(src, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	srcID, srcParentID, srcLeaf, dstParentID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	// 服务端移动仅在跨目录时执行；同目录时等价于重命名
	if srcParentID != dstParentID {
		if err := f.moveOrCopy(true, []string{srcID}, nil, dstParentID); err != nil {
			return fmt.Errorf("failed to move directory: %w", err)
		}
	}
	if srcLeaf != dstLeaf {
		if err := f.rename(ctx, 0, srcID, dstLeaf); err != nil {
			return fmt.Errorf("failed to rename moved directory: %w", err)
		}
	}
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// PublicLink generates a public link to the remote path
//
// 联通的直链无鉴权头即可访问（已验收确认），但有效期较短，
// expire 参数无法控制时效，仅返回当前可用的直链
func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	o, err := f.NewObject(ctx, remote)
	if err != nil {
		return "", err
	}
	obj, ok := o.(*Object)
	if !ok {
		return "", fmt.Errorf("can't get link - not a file: %q", remote)
	}
	return obj.getDownloadURL(ctx)
}

// DirCacheFlush resets the directory cache - used in testing
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String converts this Fs to a string
func (f *Fs) String() string {
	return fmt.Sprintf("WoPan root '%s'", f.root)
}

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Hashes returns the supported hash sets
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

// Remote returns the remote path of the object
func (o *Object) Remote() string {
	return o.remote
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

// SetModTime sets the modification time of the object
//
// WoPan does not support setting modification time: the server stamps files
// with the creation time on upload.
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Size returns the size of the object
func (o *Object) Size() int64 {
	return o.size
}

// Fs returns the Fs of the object
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Hash returns the hash of the object - WoPan provides no hashes
func (o *Object) Hash(ctx context.Context, r hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Storable returns whether the object is storable
func (o *Object) Storable() bool {
	return true
}

// Remove an object
func (o *Object) Remove(ctx context.Context) error {
	if err := o.fs.deleteFiles(ctx, nil, []string{o.id}); err != nil {
		return err
	}
	dir, _ := dircache.SplitPath(o.remote)
	o.fs.dirCache.FlushDir(dir)
	return nil
}

// String converts this object to a string
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}
