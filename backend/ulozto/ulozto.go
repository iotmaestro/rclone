package ulozto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"github.com/pkg/errors"
	"github.com/rclone/rclone/backend/ulozto/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	minSleep      = 10 * time.Millisecond
	maxSleep      = 2 * time.Second
	decayConstant = 2 // bigger for slower decay, exponential
	rootURL       = "https://apis.uloz.to"
)

type Options struct {
	AppToken       string               `config:"app_token"`
	Username       string               `config:"username"`
	Password       string               `config:"password"`
	RootFolderSlug string               `config:"root_folder_slug"`
	Enc            encoder.MultiEncoder `config:"encoding"`
	ListPageSize   int                  `config:"list_page_size"`
}

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "ulozto",
		Description: "Uloz.to",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:    "app_token",
				Default: "",
				Help: "The application token identifying the app. An app API key can be either found in the API " +
					"doc (https://uloz.to/upload-resumable-api-beta), or obtained from customer service.",
				Sensitive: true,
			},
			{
				Name:      "username",
				Default:   "",
				Help:      "The username of the principal to operate as.",
				Sensitive: true,
			},
			{
				Name:      "password",
				Default:   "",
				Help:      "The password for the user.",
				Sensitive: true,
			},
			{
				Name: "root_folder_slug",
				Help: "If set, rclone will use this folder as the root folder for all operations. For example, " +
					"if the slug identifies 'foo/bar/', 'ulozto:baz' is equivalent to 'ulozto:foo/bar/baz' without " +
					"any root slug set.",
				Default:   "",
				Advanced:  true,
				Sensitive: true,
			},
			{
				Name:     "list_page_size",
				Default:  500,
				Help:     "The size of a single page for list commands. 1-500",
				Advanced: true,
			},
			{
				Name:     config.ConfigEncoding,
				Help:     config.ConfigEncodingHelp,
				Advanced: true,
				Default:  encoder.Display | encoder.EncodeInvalidUtf8 | encoder.EncodeBackSlash,
			},
		}})
}

type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	opt      Options            // parsed options
	features *fs.Features       // optional features
	rest     *rest.Client       // REST client with authentication headers set, used to communicate with API endpoints
	cdn      *rest.Client       // REST client without authentication headers set, used for CDN payload upload/download
	dirCache *dircache.DirCache // Map of directory path to directory id
	pacer    *fs.Pacer          // pacer for API calls
}

// NewFs constructs a Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config into Options struct
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	client := fshttp.NewClient(ctx)

	f := &Fs{
		name:  name,
		root:  root,
		opt:   *opt,
		cdn:   rest.NewClient(client),
		rest:  rest.NewClient(client).SetRoot(rootURL),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(minSleep), pacer.MaxSleep(maxSleep), pacer.DecayConstant(decayConstant))),
	}
	f.features = (&fs.Features{
		DuplicateFiles:          true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	f.rest.SetErrorHandler(errorHandler)

	f.rest.SetHeader("X-Auth-Token", f.opt.AppToken)

	auth, err := f.authenticate(ctx)

	if err != nil {
		return f, err
	}

	var rootSlug string
	if opt.RootFolderSlug == "" {
		rootSlug = auth.Session.User.RootFolderSlug
	} else {
		rootSlug = opt.RootFolderSlug
	}

	f.dirCache = dircache.New(root, rootSlug, f)

	err = f.dirCache.FindRoot(ctx, false)

	if errors.Is(err, fs.ErrorDirNotFound) {
		// All good, we'll create the folder later on.
		return f, nil
	}

	if errors.Is(err, fs.ErrorIsFile) {
		rootFolder, _ := dircache.SplitPath(root)
		f.root = rootFolder
		f.dirCache = dircache.New(rootFolder, rootSlug, f)
		err = f.dirCache.FindRoot(ctx, false)
		if err != nil {
			return f, err
		}
		return f, fs.ErrorIsFile
	}

	return f, err
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	errResponse := new(api.Error)
	err := rest.DecodeJSON(resp, &errResponse)
	if err != nil {
		fs.Debugf(nil, "Couldn't decode error response: %v", err)
	}
	if errResponse.StatusCode == 0 {
		errResponse.StatusCode = resp.StatusCode
	}

	return errors.WithStack(errResponse)
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	429, // Too Many Requests.
	500, // Internal Server Error
	502, // Bad Gateway
	503, // Service Unavailable
	504, // Gateway Timeout
}

// shouldRetry returns a boolean whether this resp and err should be retried.
// It also returns the err for convenience.
func (f *Fs) shouldRetry(ctx context.Context, resp *http.Response, err error, reauth bool) (bool, error) {
	if err == nil {
		return false, nil
	}

	if fserrors.ContextError(ctx, &err) {
		return false, err
	}

	var apiErr *api.Error
	if resp != nil && resp.StatusCode == 401 && errors.As(err, &apiErr) && apiErr.ErrorCode == 70001 {
		fs.Debugf(nil, "Should retry: %v", err)

		if reauth {
			_, err = f.authenticate(ctx)
			if err != nil {
				return false, err
			}
		}

		return true, err
	}

	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

func (f *Fs) authenticate(ctx context.Context) (response *api.AuthenticateResponse, err error) {
	// TODO only reauth once if the token expires

	// Remove the old user token
	f.rest.RemoveHeader("X-User-Token")

	opts := rest.Opts{
		Method: "PUT",
		Path:   "/v6/session",
	}

	authRequest := api.AuthenticateRequest{
		Login:    f.opt.Username,
		Password: f.opt.Password,
	}

	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := f.rest.CallJSON(ctx, &opts, &authRequest, &response)
		return f.shouldRetry(ctx, httpResp, err, false)
	})

	if err != nil {
		return nil, err
	}

	f.rest.SetHeader("X-User-Token", "t9tyo9p1tbh8dbk8isejld9r3647u6")

	return response, nil
}

// UploadSession represents a single Uloz.to upload session.
//
// Uloz.to supports uploading multiple files at once and committing them atomically. This functionality isn't being used
// by the backend implementation and for simplicity, each session corresponds to a single file being uploaded.
type UploadSession struct {
	Filesystem  *Fs
	Url         string
	PrivateSlug string
	ValidUntil  time.Time
}

func (f *Fs) createUploadSession(ctx context.Context) (session *UploadSession, err error) {
	session = &UploadSession{
		Filesystem: f,
	}

	err = session.RenewUploadSession(ctx)
	if err != nil {
		return nil, err
	}

	return session, nil
}

func (session *UploadSession) RenewUploadSession(ctx context.Context) error {
	opts := rest.Opts{
		Method:     "POST",
		Path:       "/v5/upload/link",
		Parameters: url.Values{},
	}

	createUploadUrlReq := api.CreateUploadUrlRequest{
		UserLogin: session.Filesystem.opt.Username,
		Realm:     "ulozto",
	}

	if session.PrivateSlug != "" {
		createUploadUrlReq.ExistingSessionSlug = session.PrivateSlug
	}

	var err error
	var response api.CreateUploadUrlResponse

	err = session.Filesystem.pacer.Call(func() (bool, error) {
		httpResp, err := session.Filesystem.rest.CallJSON(ctx, &opts, &createUploadUrlReq, &response)
		return session.Filesystem.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return err
	}

	session.PrivateSlug = response.PrivateSlug
	session.Url = response.UploadUrl
	session.ValidUntil = response.ValidUntil

	return nil
}

func (f *Fs) uploadUnchecked(ctx context.Context, name, parentSlug string, info fs.ObjectInfo, payload io.Reader) (fs.Object, error) {
	session, err := f.createUploadSession(ctx)

	if err != nil {
		return nil, err
	}

	// If hashes are unavailable from the info, calculate them while we upload.
	hashes := hash.NewHashSet()
	sha256digest, err := info.Hash(ctx, hash.SHA256)
	if err != nil || sha256digest == "" {
		hashes.Add(hash.SHA256)
	}
	md5digest, err := info.Hash(ctx, hash.MD5)
	if err != nil || md5digest == "" {
		hashes.Add(hash.MD5)
	}
	hasher, err := hash.NewMultiHasherTypes(hashes)

	if hashes.Count() > 0 {
		payload = io.TeeReader(payload, hasher)
	}

	encodedName := f.opt.Enc.FromStandardName(name)

	opts := rest.Opts{
		Method: "POST",
		Body:   payload,
		// Not using Parameters as the session URL has parameters itself
		RootURL:              session.Url + "&batch_file_id=1&is_porn=false",
		MultipartContentName: "file",
		MultipartFileName:    encodedName,
		Parameters:           url.Values{},
	}
	var uploadResponse api.SendFilePayloadResponse

	err = f.pacer.CallNoRetry(func() (bool, error) {
		httpResp, err := f.cdn.CallJSON(ctx, &opts, nil, &uploadResponse)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return nil, err
	}

	digests := hasher.Sums()

	if hashes.Count() > 0 {
		if hashes.Contains(hash.SHA256) {
			sha256digest = digests[hash.SHA256]
		}

		if hashes.Contains(hash.MD5) {
			md5digest = digests[hash.MD5]
		}
	}

	if md5digest != uploadResponse.Md5 {
		return nil, errors.New("MD5 digest mismatch")
	}

	metadata := DescriptionEncodedMetadata{
		Md5Hash:            md5digest,
		Sha256hash:         sha256digest,
		ModTimeEpochMicros: info.ModTime(ctx).UnixMicro(),
	}

	encodedMetadata, err := metadata.Encode()

	if err != nil {
		return nil, err
	}

	// Successfully uploaded, now move the file where it belongs and commit it
	updateReq := api.BatchUpdateFilePropertiesRequest{
		Name:         encodedName,
		FolderSlug:   parentSlug,
		Description:  encodedMetadata,
		Slugs:        []string{uploadResponse.Slug},
		UploadTokens: map[string]string{uploadResponse.Slug: session.PrivateSlug + ":1"},
	}

	var updateResponse []api.File

	opts = rest.Opts{
		Method:     "PATCH",
		Path:       "/v8/file-list/private",
		Parameters: url.Values{},
	}

	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := session.Filesystem.rest.CallJSON(ctx, &opts, &updateReq, &updateResponse)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return nil, err
	}

	if len(updateResponse) != 1 {
		return nil, errors.New("unexpected number of files in the response")
	}

	opts = rest.Opts{
		Method:     "PATCH",
		Path:       "/v8/upload-batch/private/" + session.PrivateSlug,
		Parameters: url.Values{},
	}

	commitRequest := api.CommitUploadBatchRequest{
		Status:     "confirmed",
		OwnerLogin: f.opt.Username,
	}

	var commitResponse api.CommitUploadBatchResponse

	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := session.Filesystem.rest.CallJSON(ctx, &opts, &commitRequest, &commitResponse)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return nil, err
	}

	file, err := f.newObjectWithInfo(ctx, info.Remote(), &updateResponse[0])

	return file, err
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())

	switch {
	case err == nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case errors.Is(err, fs.ErrorObjectNotFound):
		// Not found so create it
		return f.PutUnchecked(ctx, in, src, options...)
	default:
		return nil, err
	}
}

func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	filename, folderSlug, err := f.dirCache.FindPath(ctx, src.Remote(), true)

	if err != nil {
		return nil, err
	}

	return f.uploadUnchecked(ctx, filename, folderSlug, src, in)
}

func (f *Fs) Mkdir(ctx context.Context, dir string) (err error) {
	_, err = f.dirCache.FindDir(ctx, dir, true)
	return err
}

func (f *Fs) isDirEmpty(ctx context.Context, slug string) (empty bool, err error) {
	folders, err := f.fetchListFolderPage(ctx, slug, "", 1, 0)

	if err != nil {
		return false, err
	}

	if len(folders) > 0 {
		return false, nil
	}

	files, err := f.fetchListFilePage(ctx, slug, "", 1, 0)

	if err != nil {
		return false, err
	}

	if len(files) > 0 {
		return false, nil
	}

	return true, nil
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	slug, err := f.dirCache.FindDir(ctx, dir, false)

	if err != nil {
		return err
	}

	empty, err := f.isDirEmpty(ctx, slug)

	if err != nil {
		return err
	}

	if !empty {
		return fs.ErrorDirectoryNotEmpty
	}

	opts := rest.Opts{
		Method: "DELETE",
		Path:   "/v5/user/" + f.opt.Username + "/folder-list",
	}

	req := api.DeleteFoldersRequest{Slugs: []string{slug}}
	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := f.rest.CallJSON(ctx, &opts, req, nil)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return err
	}

	f.dirCache.FlushDir(dir)

	return nil
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
	return fmt.Sprintf("uloz.to root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Precision return the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return time.Microsecond
}

func (f *Fs) Hashes() hash.Set {
	return hash.NewHashSet(hash.SHA256, hash.MD5)
}

// DescriptionEncodedMetadata represents a set of metadata encoded as Uloz.to description.
//
// Uloz.to doesn't support setting metadata such as mtime but allows the user to set an arbitrary description field.
// The content of this structure will be serialized and stored in the backend.
type DescriptionEncodedMetadata struct {
	Md5Hash            string
	Sha256hash         string
	ModTimeEpochMicros int64
}

func (md *DescriptionEncodedMetadata) Encode() (string, error) {
	b := bytes.Buffer{}
	e := gob.NewEncoder(&b)
	err := e.Encode(md)
	if err != nil {
		return "", err
	}
	// Version the encoded string from the beginning even though we don't need it yet.
	return "1;" + base64.StdEncoding.EncodeToString(b.Bytes()), nil
}

func DecodeDescriptionMetadata(str string) (*DescriptionEncodedMetadata, error) {
	// The encoded data starts with a version number which is not a part iof the serialized object
	spl := strings.SplitN(str, ";", 2)

	if len(spl) < 2 || spl[0] != "1" {
		return nil, errors.New("can't decode, unknown encoded metadata version")
	}

	m := DescriptionEncodedMetadata{}
	by, err := base64.StdEncoding.DecodeString(spl[1])
	if err != nil {
		return nil, err
	}
	b := bytes.Buffer{}
	b.Write(by)
	d := gob.NewDecoder(&b)
	err = d.Decode(&m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

type Object struct {
	fs              *Fs                         // what this object is part of
	remote          string                      // The remote path
	name            string                      // The file name
	size            int64                       // size of the object
	slug            string                      // ID of the object
	encodedMetadata *DescriptionEncodedMetadata // Metadata not available natively and encoded in the description field
}

func (o *Object) Storable() bool {
	return true
}

func (o *Object) updateFileProperties(ctx context.Context, req interface{}) (err error) {
	var resp *api.File

	opts := rest.Opts{
		Method: "PATCH",
		Path:   "/v8/file/" + o.slug + "/private",
	}

	err = o.fs.pacer.Call(func() (bool, error) {
		httpResp, err := o.fs.rest.CallJSON(ctx, &opts, &req, &resp)
		return o.fs.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return err
	}

	return o.setMetaData(resp)
}

func (o *Object) SetModTime(ctx context.Context, t time.Time) (err error) {
	newMetadata := *o.encodedMetadata
	newMetadata.ModTimeEpochMicros = t.UnixMicro()
	encoded, err := newMetadata.Encode()
	if err != nil {
		return err
	}
	return o.updateFileProperties(ctx, api.UpdateDescriptionRequest{
		Description: encoded,
	})
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (rc io.ReadCloser, err error) {
	opts := rest.Opts{
		Method: "POST",
		Path:   "/v5/file/download-link/vipdata",
	}

	req := &api.GetDownloadLinkRequest{
		Slug:      o.slug,
		UserLogin: o.fs.opt.Username,
		// Has to be set but doesn't seem to be used server side.
		DeviceId: "foobar",
	}

	var resp *api.GetDownloadLinkResponse

	err = o.fs.pacer.Call(func() (bool, error) {
		httpResp, err := o.fs.rest.CallJSON(ctx, &opts, &req, &resp)
		return o.fs.shouldRetry(ctx, httpResp, err, true)
	})
	if err != nil {
		return nil, err
	}

	opts = rest.Opts{
		Method:  "GET",
		RootURL: resp.Link,
		Options: options,
	}

	var httpResp *http.Response

	err = o.fs.pacer.Call(func() (bool, error) {
		httpResp, err = o.fs.cdn.Call(ctx, &opts)
		return o.fs.shouldRetry(ctx, httpResp, err, true)
	})
	if err != nil {
		return nil, err
	}
	return httpResp.Body, err
}

func (o *Object) copyFrom(other *Object) {
	o.fs = other.fs
	o.remote = other.remote
	o.size = other.size
	o.slug = other.slug
	o.encodedMetadata = other.encodedMetadata
}

type RenamingObjectInfoProxy struct {
	delegate fs.ObjectInfo
	remote   string
}

func (s *RenamingObjectInfoProxy) String() string {
	return s.delegate.String()
}

func (s *RenamingObjectInfoProxy) Remote() string {
	return s.remote
}

func (s *RenamingObjectInfoProxy) ModTime(ctx context.Context) time.Time {
	return s.delegate.ModTime(ctx)
}

func (s *RenamingObjectInfoProxy) Size() int64 {
	return s.delegate.Size()
}

func (s *RenamingObjectInfoProxy) Fs() fs.Info {
	return s.delegate.Fs()
}

func (s *RenamingObjectInfoProxy) Hash(ctx context.Context, ty hash.Type) (string, error) {
	return s.delegate.Hash(ctx, ty)
}

func (s *RenamingObjectInfoProxy) Storable() bool {
	return s.delegate.Storable()
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// The backend allows to store multiple files with the same name, so simply upload the new file and remove the old
	// one afterwards.
	info := &RenamingObjectInfoProxy{
		delegate: src,
		remote:   o.Remote(),
	}
	newo, err := o.fs.PutUnchecked(ctx, in, info, options...)

	if err != nil {
		return err
	}

	err = o.Remove(ctx)
	if err != nil {
		return err
	}

	o.copyFrom(newo.(*Object))

	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	for i := 0; i < 2; i++ {
		// First call moves the item to recycle bin, second deletes it for good
		var err error
		opts := rest.Opts{
			Method: "DELETE",
			Path:   "/v6/file/" + o.slug + "/private",
		}
		err = o.fs.pacer.Call(func() (bool, error) {
			httpResp, err := o.fs.rest.CallJSON(ctx, &opts, nil, nil)
			return o.fs.shouldRetry(ctx, httpResp, err, true)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (o *Object) ModTime(ctx context.Context) time.Time {
	return time.UnixMicro(o.encodedMetadata.ModTimeEpochMicros)
}

func (o *Object) Fs() fs.Info {
	return o.fs
}

// Return a string version
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.size
}

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	switch t {
	case hash.MD5:
		return o.encodedMetadata.Md5Hash, nil
	case hash.SHA256:
		return o.encodedMetadata.Sha256hash, nil
	default:
		return "", hash.ErrUnsupported
	}
}

func (f *Fs) FindLeaf(ctx context.Context, folderSlug, leaf string) (leafSlug string, found bool, err error) {
	folders, err := f.listFolders(ctx, folderSlug, leaf)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return "", false, nil
		} else {
			return "", false, err
		}
	}

	for _, folder := range folders {
		if folder.Name == leaf {
			return folder.Slug, true, nil
		}
	}

	// Uloz.to allows creation of multiple files / folders with the same name in the same parent folder. rclone always
	// expects folder paths to be unique (no other file or folder with the same name should exist). As a result we also
	// need to look at the files to return the correct error if necessary.
	files, err := f.listFiles(ctx, folderSlug, leaf)
	if err != nil {
		return "", false, err
	}

	for _, file := range files {
		if file.Name == leaf {
			return "", false, fs.ErrorIsFile
		}
	}

	// The parent folder exists but no file or folder with the given name was found in it.
	return "", false, nil
}

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	var folder *api.Folder
	opts := rest.Opts{
		Method:     "POST",
		Path:       "/v6/user/" + f.opt.Username + "/folder",
		Parameters: url.Values{},
	}
	mkdir := api.CreateFolderRequest{
		Name:             f.opt.Enc.FromStandardName(leaf),
		ParentFolderSlug: pathID,
	}
	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := f.rest.CallJSON(ctx, &opts, &mkdir, &folder)
		return f.shouldRetry(ctx, httpResp, err, true)
	})
	if err != nil {
		return "", err
	}
	return folder.Slug, nil
}

func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *api.File) (*Object, error) {
	o := &Object{
		fs:     f,
		remote: remote,
	}
	var err error

	if info == nil {
		info, err = f.readMetaDataForPath(ctx, remote)
	}

	if err != nil {
		return nil, err
	}

	err = o.setMetaData(info)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// readMetaDataForPath reads the metadata from the path
func (f *Fs) readMetaDataForPath(ctx context.Context, path string) (info *api.File, err error) {
	filename, folderSlug, err := f.dirCache.FindPath(ctx, path, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}

	files, err := f.listFiles(ctx, folderSlug, filename)

	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if file.Name == filename {
			return &file, nil
		}
	}

	folders, err := f.listFolders(ctx, folderSlug, filename)

	if err != nil {
		return nil, err
	}

	for _, file := range folders {
		if file.Name == filename {
			return nil, fs.ErrorIsDir
		}
	}

	return nil, fs.ErrorObjectNotFound
}

func (o *Object) setMetaData(info *api.File) (err error) {
	o.name = info.Name
	o.size = info.Filesize
	o.encodedMetadata, err = DecodeDescriptionMetadata(info.Description)
	if err != nil {
		return err
	}
	o.slug = info.Slug
	return nil
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.newObjectWithInfo(ctx, remote, nil)
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	folderSlug, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}

	folders, err := f.listFolders(ctx, folderSlug, "")
	if err != nil {
		return nil, err
	}

	for _, folder := range folders {
		remote := path.Join(dir, folder.Name)
		f.dirCache.Put(remote, folder.Slug)
		entries = append(entries, fs.NewDir(remote, folder.LastUserModified))
	}

	files, err := f.listFiles(ctx, folderSlug, "")
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		remote := path.Join(dir, file.Name)
		remoteFile, err := f.newObjectWithInfo(ctx, remote, &file)
		if err != nil {
			return nil, err
		}
		entries = append(entries, remoteFile)
	}

	return entries, nil
}

func (f *Fs) fetchListFolderPage(
	ctx context.Context,
	folderSlug string,
	searchQuery string,
	limit int,
	offset int) (folders []api.Folder, err error) {

	opts := rest.Opts{
		Method:     "GET",
		Path:       "/v9/user/" + f.opt.Username + "/folder/" + folderSlug + "/folder-list",
		Parameters: url.Values{},
	}

	opts.Parameters.Set("status", "ok")
	opts.Parameters.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		opts.Parameters.Set("offset", strconv.Itoa(offset))
	}

	if searchQuery != "" {
		opts.Parameters.Set("search_query", f.opt.Enc.FromStandardName(searchQuery))
	}

	var respBody *api.ListFoldersResponse

	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := f.rest.CallJSON(ctx, &opts, nil, &respBody)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return nil, err
	}

	for i := range respBody.Subfolders {
		respBody.Subfolders[i].Name = f.opt.Enc.ToStandardName(respBody.Subfolders[i].Name)
	}

	return respBody.Subfolders, nil
}

func (f *Fs) listFolders(
	ctx context.Context,
	folderSlug string,
	searchQuery string) (folders []api.Folder, err error) {

	targetPageSize := f.opt.ListPageSize
	lastPageSize := targetPageSize
	offset := 0

	for targetPageSize == lastPageSize {
		page, err := f.fetchListFolderPage(ctx, folderSlug, searchQuery, targetPageSize, offset)
		if err != nil {
			var apiErr *api.Error
			casted := errors.As(err, &apiErr)
			if casted && apiErr.ErrorCode == 30001 {
				return nil, fs.ErrorDirNotFound
			}
			return nil, err
		}
		lastPageSize = len(page)
		offset += lastPageSize
		folders = append(folders, page...)
	}

	return folders, nil
}

func (f *Fs) fetchListFilePage(
	ctx context.Context,
	folderSlug string,
	searchQuery string,
	limit int,
	offset int) (folders []api.File, err error) {

	opts := rest.Opts{
		Method:     "GET",
		Path:       "/v8/user/" + f.opt.Username + "/folder/" + folderSlug + "/file-list",
		Parameters: url.Values{},
	}
	opts.Parameters.Set("status", "ok")
	opts.Parameters.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		opts.Parameters.Set("offset", strconv.Itoa(offset))
	}

	if searchQuery != "" {
		opts.Parameters.Set("search_query", f.opt.Enc.FromStandardName(searchQuery))
	}

	var respBody *api.ListFilesResponse

	err = f.pacer.Call(func() (bool, error) {
		httpResp, err := f.rest.CallJSON(ctx, &opts, nil, &respBody)
		return f.shouldRetry(ctx, httpResp, err, true)
	})

	if err != nil {
		return nil, fmt.Errorf("couldn't list files: %w", err)
	}

	for i := range respBody.Items {
		respBody.Items[i].Name = f.opt.Enc.ToStandardName(respBody.Items[i].Name)
	}

	return respBody.Items, nil
}

func (f *Fs) listFiles(
	ctx context.Context,
	folderSlug string,
	searchQuery string) (folders []api.File, err error) {

	targetPageSize := f.opt.ListPageSize
	lastPageSize := targetPageSize
	offset := 0

	for targetPageSize == lastPageSize {
		page, err := f.fetchListFilePage(ctx, folderSlug, searchQuery, targetPageSize, offset)
		if err != nil {
			return nil, err
		}
		lastPageSize = len(page)
		offset += lastPageSize
		folders = append(folders, page...)
	}

	return folders, nil
}

// DirCacheFlush resets the directory cache - used in testing as an
// optional interface
func (f *Fs) DirCacheFlush() {
	f.dirCache.ResetRoot()
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.ObjectInfo      = (*RenamingObjectInfoProxy)(nil)
)
