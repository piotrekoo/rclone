package opendrive

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"fmt"

	"github.com/ncw/rclone/dircache"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/pacer"
	"github.com/ncw/rclone/rest"
	"github.com/pkg/errors"
)

const (
	defaultEndpoint = "https://dev.opendrive.com/api/v1"
	minSleep        = 10 * time.Millisecond
	maxSleep        = 5 * time.Minute
	decayConstant   = 1 // bigger for slower decay, exponential
	maxParts        = 10000
	maxVersions     = 100 // maximum number of versions we search in --b2-versions mode
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "opendrive",
		Description: "OpenDrive",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name: "username",
			Help: "Username",
		}, {
			Name:       "password",
			Help:       "Password.",
			IsPassword: true,
		}},
	})
}

// Fs represents a remote b2 server
type Fs struct {
	name     string             // name of this remote
	root     string             // the path we are working on
	features *fs.Features       // optional features
	username string             // account name
	password string             // auth key0
	srv      *rest.Client       // the connection to the b2 server
	pacer    *pacer.Pacer       // To pace and retry the API calls
	session  UserSessionInfo    // contains the session data
	dirCache *dircache.DirCache // Map of directory path to directory id
}

// Object describes a b2 object
type Object struct {
	fs      *Fs       // what this object is part of
	remote  string    // The remote path
	id      string    // b2 id of the file
	modTime time.Time // The modified time of the object if known
	md5     string    // MD5 hash if known
	size    int64     // Size of the object
}

// parsePath parses an acd 'url'
func parsePath(path string) (root string) {
	root = strings.Trim(path, "/")
	return
}

// mimics url.PathEscape which only available from go 1.8
func pathEscape(path string) string {
	u := url.URL{
		Path: path,
	}
	return u.EscapedPath()
}

// ------------------------------------------------------------

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
	return fmt.Sprintf("OpenDrive root '%s'", f.root)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Hashes returns the supported hash sets.
func (f *Fs) Hashes() fs.HashSet {
	return fs.HashSet(fs.HashMD5)
}

// NewFs contstructs an Fs from the path, bucket:path
func NewFs(name, root string) (fs.Fs, error) {
	root = parsePath(root)
	fs.Debugf(nil, "NewFS(\"%s\", \"%s\"", name, root)
	username := fs.ConfigFileGet(name, "username")
	if username == "" {
		return nil, errors.New("username not found")
	}
	password, err := fs.Reveal(fs.ConfigFileGet(name, "password"))
	if err != nil {
		return nil, errors.New("password coudl not revealed")
	}
	if password == "" {
		return nil, errors.New("password not found")
	}

	fs.Debugf(nil, "OpenDrive-user: %s", username)
	fs.Debugf(nil, "OpenDrive-pass: %s", password)

	f := &Fs{
		name:     name,
		username: username,
		password: password,
		root:     root,
		srv:      rest.NewClient(fs.Config.Client()).SetErrorHandler(errorHandler),
		pacer:    pacer.New().SetMinSleep(minSleep).SetMaxSleep(maxSleep).SetDecayConstant(decayConstant),
	}

	f.dirCache = dircache.New(root, "0", f)

	// set the rootURL for the REST client
	f.srv.SetRoot(defaultEndpoint)

	// get sessionID
	var resp *http.Response
	err = f.pacer.Call(func() (bool, error) {
		account := Account{Username: username, Password: password}

		opts := rest.Opts{
			Method: "POST",
			Path:   "/session/login.json",
		}
		resp, err = f.srv.CallJSON(&opts, &account, &f.session)
		return f.shouldRetry(resp, err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create session")
	}

	fs.Debugf(nil, "Starting OpenDrive session with ID: %s", f.session.SessionID)

	f.features = (&fs.Features{ReadMimeType: true, WriteMimeType: true}).Fill(f)

	// Find the current root
	err = f.dirCache.FindRoot(false)
	if err != nil {
		// Assume it is a file
		newRoot, remote := dircache.SplitPath(root)
		newF := *f
		newF.dirCache = dircache.New(newRoot, "0", &newF)
		newF.root = newRoot

		// Make new Fs which is the parent
		err = newF.dirCache.FindRoot(false)
		if err != nil {
			// No root so return old f
			return f, nil
		}
		_, err := newF.newObjectWithInfo(remote, nil)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				// File doesn't exist so return old f
				return f, nil
			}
			return nil, err
		}
		// return an error with an fs which points to the parent
		return &newF, fs.ErrorIsFile
	}
	return f, nil
}

// rootSlash returns root with a slash on if it is empty, otherwise empty string
func (f *Fs) rootSlash() string {
	if f.root == "" {
		return f.root
	}
	return f.root + "/"
}

// errorHandler parses a non 2xx error response into an error
func errorHandler(resp *http.Response) error {
	// Decode error response
	// errResponse := new(api.Error)
	// err := rest.DecodeJSON(resp, &errResponse)
	// if err != nil {
	// 	fs.Debugf(nil, "Couldn't decode error response: %v", err)
	// }
	// if errResponse.Code == "" {
	// 	errResponse.Code = "unknown"
	// }
	// if errResponse.Status == 0 {
	// 	errResponse.Status = resp.StatusCode
	// }
	// if errResponse.Message == "" {
	// 	errResponse.Message = "Unknown " + resp.Status
	// }
	// return errResponse
	return nil
}

// Mkdir creates the folder if it doesn't exist
func (f *Fs) Mkdir(dir string) error {
	fs.Debugf(nil, "Mkdir(\"%s\")", dir)
	err := f.dirCache.FindRoot(true)
	if err != nil {
		return err
	}
	if dir != "" {
		_, err = f.dirCache.FindDir(dir, true)
	}
	return err
}

// deleteObject removes an object by ID
func (f *Fs) deleteObject(id string) error {
	return f.pacer.Call(func() (bool, error) {
		removeDirData := removeFolder{SessionID: f.session.SessionID, FolderID: id}
		opts := rest.Opts{
			Method: "POST",
			Path:   "/folder/remove.json",
		}
		resp, err := f.srv.CallJSON(&opts, &removeDirData, nil)
		return f.shouldRetry(resp, err)
	})
}

// purgeCheck remotes the root directory, if check is set then it
// refuses to do so if it has anything in
func (f *Fs) purgeCheck(dir string, check bool) error {
	root := path.Join(f.root, dir)
	if root == "" {
		return errors.New("can't purge root directory")
	}
	dc := f.dirCache
	err := dc.FindRoot(false)
	if err != nil {
		return err
	}
	rootID, err := dc.FindDir(dir, false)
	if err != nil {
		return err
	}
	item, _, err := f.readMetaDataForFolderID(rootID)
	if err != nil {
		return err
	}
	if check && len(item.Files) != 0 {
		return errors.New("folder not empty")
	}
	err = f.deleteObject(rootID)
	if err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	if err != nil {
		return err
	}
	return nil
}

// Rmdir deletes the root folder
//
// Returns an error if it isn't empty
func (f *Fs) Rmdir(dir string) error {
	fs.Debugf(nil, "Rmdir(\"%s\")", path.Join(f.root, dir))
	return f.purgeCheck(dir, true)
}

// Precision of the remote
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Copy src to this remote using server side copy operations.
//
// This is stored with the remote path given
//
// It returns the destination Object and a possible error
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantCopy
func (f *Fs) Copy(src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(nil, "Copy(%v)", remote)
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't copy - not same remote type")
		return nil, fs.ErrorCantCopy
	}
	err := srcObj.readMetaData()
	if err != nil {
		return nil, err
	}

	srcPath := srcObj.fs.rootSlash() + srcObj.remote
	dstPath := f.rootSlash() + remote
	if strings.ToLower(srcPath) == strings.ToLower(dstPath) {
		return nil, errors.Errorf("Can't copy %q -> %q as are same name when lowercase", srcPath, dstPath)
	}

	// Create temporary object
	dstObj, _, directoryID, err := f.createObject(remote, srcObj.modTime, srcObj.size)
	if err != nil {
		return nil, err
	}

	// Copy the object
	var resp *http.Response
	response := copyFileResponse{}
	err = f.pacer.Call(func() (bool, error) {
		copyFileData := copyFile{
			SessionID:         f.session.SessionID,
			SrcFileID:         srcObj.id,
			DstFolderID:       directoryID,
			Move:              "false",
			OverwriteIfExists: "true",
		}
		opts := rest.Opts{
			Method: "POST",
			Path:   "/file/move_copy.json",
		}
		resp, err = f.srv.CallJSON(&opts, &copyFileData, &response)
		return f.shouldRetry(resp, err)
	})
	if err != nil {
		return nil, err
	}

	size, _ := strconv.ParseInt(response.Size, 10, 64)
	dstObj.id = response.FileID
	dstObj.size = size

	return dstObj, nil
}

// Purge deletes all the files and the container
//
// Optional interface: Only implement this if you have a way of
// deleting all the files quicker than just running Remove() on the
// result of List()
func (f *Fs) Purge() error {
	return f.purgeCheck("", false)
}

// Return an Object from a path
//
// If it can't be found it returns the error fs.ErrorObjectNotFound.
func (f *Fs) newObjectWithInfo(remote string, file *File) (fs.Object, error) {
	fs.Debugf(nil, "newObjectWithInfo(%s, %v)", remote, file)

	var o *Object
	if nil != file {
		o = &Object{
			fs:      f,
			remote:  remote,
			id:      file.FileID,
			modTime: time.Unix(file.DateModified, 0),
			size:    file.Size,
		}
	} else {
		o = &Object{
			fs:     f,
			remote: remote,
		}

		err := o.readMetaData()
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(remote string) (fs.Object, error) {
	fs.Debugf(nil, "NewObject(\"%s\")", remote)
	return f.newObjectWithInfo(remote, nil)
}

// Creates from the parameters passed in a half finished Object which
// must have setMetaData called on it
//
// Returns the object, leaf, directoryID and error
//
// Used to create new objects
func (f *Fs) createObject(remote string, modTime time.Time, size int64) (o *Object, leaf string, directoryID string, err error) {
	// Create the directory for the object if it doesn't exist
	leaf, directoryID, err = f.dirCache.FindRootAndPath(remote, true)
	if err != nil {
		return nil, leaf, directoryID, err
	}
	// Temporary Object under construction
	o = &Object{
		fs:     f,
		remote: remote,
	}
	return o, leaf, directoryID, nil
}

// readMetaDataForPath reads the metadata from the path
func (f *Fs) readMetaDataForFolderID(id string) (info *FolderList, resp *http.Response, err error) {
	opts := rest.Opts{
		Method: "GET",
		Path:   "/folder/list.json/" + f.session.SessionID + "/" + id,
	}
	err = f.pacer.Call(func() (bool, error) {
		resp, err = f.srv.CallJSON(&opts, nil, &info)
		return f.shouldRetry(resp, err)
	})
	return info, resp, err
}

// Put the object into the bucket
//
// Copy the reader in to the new object which is returned
//
// The new object may have been created if an error is returned
func (f *Fs) Put(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()
	modTime := src.ModTime()

	fs.Debugf(nil, "Put(%s)", remote)

	o, leaf, directoryID, err := f.createObject(remote, modTime, size)
	if err != nil {
		return nil, err
	}

	if "" == o.id {
		o.readMetaData()
	}

	if "" == o.id {
		// We need to create a ID for this file
		var resp *http.Response
		response := createFileResponse{}
		err := o.fs.pacer.Call(func() (bool, error) {
			createFileData := createFile{SessionID: o.fs.session.SessionID, FolderID: directoryID, Name: replaceReservedChars(leaf)}
			opts := rest.Opts{
				Method: "POST",
				Path:   "/upload/create_file.json",
			}
			resp, err = o.fs.srv.CallJSON(&opts, &createFileData, &response)
			return o.fs.shouldRetry(resp, err)
		})
		if err != nil {
			return nil, errors.Wrap(err, "failed to create file")
		}

		o.id = response.FileID
	}

	return o, o.Update(in, src, options...)
}

// retryErrorCodes is a slice of error codes that we will retry
var retryErrorCodes = []int{
	400, // Bad request (seen in "Next token is expired")
	401, // Unauthorized (seen in "Token has expired")
	408, // Request Timeout
	429, // Rate exceeded.
	500, // Get occasional 500 Internal Server Error
	502, // Bad Gateway when doing big listings
	503, // Service Unavailable
	504, // Gateway Time-out
}

// shouldRetry returns a boolean as to whether this resp and err
// deserve to be retried.  It returns the err as a convenience
func (f *Fs) shouldRetry(resp *http.Response, err error) (bool, error) {
	return fs.ShouldRetry(err) || fs.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// DirCacher methods

// CreateDir makes a directory with pathID as parent and name leaf
func (f *Fs) CreateDir(pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "CreateDir(%q, %q)\n", pathID, replaceReservedChars(leaf))
	var resp *http.Response
	response := createFolderResponse{}
	err = f.pacer.Call(func() (bool, error) {
		createDirData := createFolder{SessionID: f.session.SessionID, FolderName: replaceReservedChars(leaf), FolderSubParent: pathID}
		opts := rest.Opts{
			Method: "POST",
			Path:   "/folder.json",
		}
		resp, err = f.srv.CallJSON(&opts, &createDirData, &response)
		return f.shouldRetry(resp, err)
	})
	if err != nil {
		// fmt.Printf("...Error %v\n", err)
		return "", err
	}
	// fmt.Printf("...Id %q\n", response.FolderID)
	return response.FolderID, nil
}

// FindLeaf finds a directory of name leaf in the folder with ID pathID
func (f *Fs) FindLeaf(pathID, leaf string) (pathIDOut string, found bool, err error) {
	fs.Debugf(nil, "FindLeaf(\"%s\", \"%s\")", pathID, leaf)

	if pathID == "0" && leaf == "" {
		fs.Debugf(nil, "Found OpenDrive root")
		// that's the root directory
		return pathID, true, nil
	}

	// get the folderIDs
	var resp *http.Response
	folderList := FolderList{}
	err = f.pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method: "GET",
			Path:   "/folder/list.json/" + f.session.SessionID + "/" + pathID,
		}
		resp, err = f.srv.CallJSON(&opts, nil, &folderList)
		return f.shouldRetry(resp, err)
	})
	if err != nil {
		return "", false, errors.Wrap(err, "failed to get folder list")
	}

	for _, folder := range folderList.Folders {
		fs.Debugf(nil, "Folder: %s (%s)", folder.Name, folder.FolderID)

		if leaf == folder.Name {
			// found
			return folder.FolderID, true, nil
		}
	}

	return "", false, nil
}

// List walks the path returning files and directories into out
func (f *Fs) List(out fs.ListOpts, dir string) {
	f.dirCache.List(f, out, dir)
}

// ListDir reads the directory specified by the job into out, returning any more jobs
func (f *Fs) ListDir(out fs.ListOpts, job dircache.ListDirJob) (jobs []dircache.ListDirJob, err error) {
	fs.Debugf(nil, "ListDir(%v, %v)", out, job)
	// get the folderIDs
	var resp *http.Response
	folderList := FolderList{}
	err = f.pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method: "GET",
			Path:   "/folder/list.json/" + f.session.SessionID + "/" + job.DirID,
		}
		resp, err = f.srv.CallJSON(&opts, nil, &folderList)
		return f.shouldRetry(resp, err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get folder list")
	}

	for _, folder := range folderList.Folders {
		folder.Name = restoreReservedChars(folder.Name)
		fs.Debugf(nil, "Folder: %s (%s)", folder.Name, folder.FolderID)
		remote := job.Path + folder.Name
		if out.IncludeDirectory(remote) {
			dir := &fs.Dir{
				Name:  remote,
				Bytes: -1,
				Count: -1,
			}
			dir.When = time.Unix(int64(folder.DateModified), 0)
			if out.AddDir(dir) {
				continue
			}
			if job.Depth > 0 {
				jobs = append(jobs, dircache.ListDirJob{DirID: folder.FolderID, Path: remote + "/", Depth: job.Depth - 1})
			}
		}
	}

	for _, file := range folderList.Files {
		file.Name = restoreReservedChars(file.Name)
		fs.Debugf(nil, "File: %s (%s)", file.Name, file.FileID)
		remote := job.Path + file.Name
		o, err := f.newObjectWithInfo(remote, &file)
		if err != nil {
			out.SetError(err)
			continue
		}
		out.Add(o)
	}

	return jobs, nil
}

// ------------------------------------------------------------

// Fs returns the parent Fs
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

// Hash returns the Md5sum of an object returning a lowercase hex string
func (o *Object) Hash(t fs.HashType) (string, error) {
	if t != fs.HashMD5 {
		return "", fs.ErrHashUnsupported
	}
	return o.md5, nil
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.size // Object is likely PENDING
}

// ModTime returns the modification time of the object
//
//
// It attempts to read the objects mtime and if that isn't present the
// LastModified returned in the http headers
func (o *Object) ModTime() time.Time {
	return o.modTime
}

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(modTime time.Time) error {
	fs.Debugf(nil, "SetModTime(%v)", modTime.String())
	opts := rest.Opts{
		Method: "PUT",
		Path: "/file/filesettings.json",
	}
	update := modTimeFile{SessionID: o.fs.session.SessionID, FileID: o.id, FileModificationTime: strconv.FormatInt(modTime.Unix(), 10)}
	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(&opts, &update, nil)
		return o.fs.shouldRetry(resp, err)
	})
	return err
}

// Open an object for read
func (o *Object) Open(options ...fs.OpenOption) (in io.ReadCloser, err error) {
	fs.Debugf(nil, "Open(\"%v\")", o.remote)

	// get the folderIDs
	var resp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method: "GET",
			Path:   "/download/file.json/" + o.id + "?session_id=" + o.fs.session.SessionID,
		}
		resp, err = o.fs.srv.Call(&opts)
		return o.fs.shouldRetry(resp, err)
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file)")
	}

	return resp.Body, nil
}

// Remove an object
func (o *Object) Remove() error {
	fs.Debugf(nil, "Remove(\"%s\")", o.id)
	return o.fs.pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method: "DELETE",
			Path:   "/file.json/" + o.fs.session.SessionID + "/" + o.id,
		}
		resp, err := o.fs.srv.Call(&opts)
		return o.fs.shouldRetry(resp, err)
	})
}

// Storable returns a boolean showing whether this object storable
func (o *Object) Storable() bool {
	return true
}

// Update the object with the contents of the io.Reader, modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	size := src.Size()
	modTime := src.ModTime()
	fs.Debugf(nil, "Update(\"%s\", \"%s\")", o.id, o.remote)

	// Open file for upload
	var resp *http.Response
	openResponse := openUploadResponse{}
	err := o.fs.pacer.Call(func() (bool, error) {
		openUploadData := openUpload{SessionID: o.fs.session.SessionID, FileID: o.id, Size: size}
		fs.Debugf(nil, "PreOpen: %#v", openUploadData)
		opts := rest.Opts{
			Method: "POST",
			Path:   "/upload/open_file_upload.json",
		}
		resp, err := o.fs.srv.CallJSON(&opts, &openUploadData, &openResponse)
		return o.fs.shouldRetry(resp, err)
	})
	if err != nil {
		return errors.Wrap(err, "failed to create file")
	}
	fs.Debugf(nil, "PostOpen: %#v", openResponse)

	// 1 MB chunks size
	chunkSize := int64(1024 * 1024 * 10)
	chunkOffset := int64(0)
	remainingBytes := size
	chunkCounter := 0

	for remainingBytes > 0 {
		currentChunkSize := chunkSize
		if currentChunkSize > remainingBytes {
			currentChunkSize = remainingBytes
		}
		remainingBytes -= currentChunkSize
		fs.Debugf(nil, "Chunk %d: size=%d, remain=%d", chunkCounter, currentChunkSize, remainingBytes)

		err = o.fs.pacer.Call(func() (bool, error) {
			var formBody bytes.Buffer
			w := multipart.NewWriter(&formBody)
			fw, err := w.CreateFormFile("file_data", o.remote)
			if err != nil {
				return false, err
			}
			if _, err = io.CopyN(fw, in, currentChunkSize); err != nil {
				return false, err
			}
			// Add session_id
			if fw, err = w.CreateFormField("session_id"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(o.fs.session.SessionID)); err != nil {
				return false, err
			}
			// Add session_id
			if fw, err = w.CreateFormField("session_id"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(o.fs.session.SessionID)); err != nil {
				return false, err
			}
			// Add file_id
			if fw, err = w.CreateFormField("file_id"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(o.id)); err != nil {
				return false, err
			}
			// Add temp_location
			if fw, err = w.CreateFormField("temp_location"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(openResponse.TempLocation)); err != nil {
				return false, err
			}
			// Add chunk_offset
			if fw, err = w.CreateFormField("chunk_offset"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(strconv.FormatInt(chunkOffset, 10))); err != nil {
				return false, err
			}
			// Add chunk_size
			if fw, err = w.CreateFormField("chunk_size"); err != nil {
				return false, err
			}
			if _, err = fw.Write([]byte(strconv.FormatInt(currentChunkSize, 10))); err != nil {
				return false, err
			}
			// Don't forget to close the multipart writer.
			// If you don't close it, your request will be missing the terminating boundary.
			w.Close()

			opts := rest.Opts{
				Method:       "POST",
				Path:         "/upload/upload_file_chunk.json",
				Body:         &formBody,
				ExtraHeaders: map[string]string{"Content-Type": w.FormDataContentType()},
			}
			resp, err = o.fs.srv.Call(&opts)
			return o.fs.shouldRetry(resp, err)
		})
		if err != nil {
			return errors.Wrap(err, "failed to create file")
		}

		resp.Body.Close()

		chunkCounter++
		chunkOffset += currentChunkSize
	}

	// Close file for upload
	closeResponse := closeUploadResponse{}
	err = o.fs.pacer.Call(func() (bool, error) {
		closeUploadData := closeUpload{SessionID: o.fs.session.SessionID, FileID: o.id, Size: size, TempLocation: openResponse.TempLocation}
		fs.Debugf(nil, "PreClose: %s", closeUploadData)
		opts := rest.Opts{
			Method: "POST",
			Path:   "/upload/close_file_upload.json",
		}
		resp, err = o.fs.srv.CallJSON(&opts, &closeUploadData, &closeResponse)
		return o.fs.shouldRetry(resp, err)
	})
	if err != nil {
		return errors.Wrap(err, "failed to create file")
	}
	fs.Debugf(nil, "PostClose: %#v", closeResponse)

	o.id = closeResponse.FileID
	o.size = closeResponse.Size
	o.modTime = modTime

	// Set the mod time now and read metadata
	err = o.SetModTime(modTime)
	if err != nil {
		return err
	}

	return nil
}

func (o *Object) readMetaData() (err error) {
	leaf, directoryID, err := o.fs.dirCache.FindRootAndPath(o.remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return fs.ErrorObjectNotFound
		}
		return err
	}
	var resp *http.Response
	folderList := FolderList{}
	err = o.fs.pacer.Call(func() (bool, error) {
		opts := rest.Opts{
			Method: "GET",
			Path:   "/folder/itembyname.json/" + o.fs.session.SessionID + "/" + directoryID + "?name=" + pathEscape(replaceReservedChars(leaf)),
		}
		resp, err = o.fs.srv.CallJSON(&opts, nil, &folderList)
		return o.fs.shouldRetry(resp, err)
	})
	if err != nil {
		return errors.Wrap(err, "failed to get folder list")
	}

	if len(folderList.Files) == 0 {
		return fs.ErrorObjectNotFound
	}

	leafFile := folderList.Files[0]
	o.id = leafFile.FileID
	o.modTime = time.Unix(leafFile.DateModified, 0)
	o.md5 = ""
	o.size = leafFile.Size

	return nil
}