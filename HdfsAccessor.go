// Copyright (c) Microsoft. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for details.
package main

import (
	"bazil.org/fuse"
	"errors"
	"fmt"
	"github.com/colinmarc/hdfs"
	"github.com/colinmarc/hdfs/protocol/hadoop_hdfs"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Interface for accessing HDFS
// Concurrency: thread safe: handles unlimited number of concurrent requests
type HdfsAccessor interface {
	OpenRead(path string) (ReadSeekCloser, error)                 // Opens HDFS file for reading
	CreateFile(path string, mode os.FileMode) (HdfsWriter, error) // Opens HDFS file for writing
	ReadDir(path string) ([]Attrs, error)                         // Enumerates HDFS directory
	Stat(path string) (Attrs, error)                              // Retrieves file/directory attributes
	StatFs() (FsInfo, error)                                      // Retrieves HDFS usage
	Mkdir(path string, mode os.FileMode) error                    // Creates a directory
	Remove(path string) error                                     // Removes a file or directory
	Rename(oldPath string, newPath string) error                  // Renames a file or directory
	EnsureConnected() error                                       // Ensures HDFS accessor is connected to the HDFS name node
	Chown(path string, owner, group string) error                 // Changes the owner and group of the file
	Chmod(path string, mode os.FileMode) error                    // Changes the mode of the file
	Close() error                                                 // Close current meta connection if needed
}

type hdfsAccessorImpl struct {
	Clock               Clock                    // interface to get wall clock time
	NameNodeAddresses   []string                 // array of Address:port string for the name nodes
	MetadataClient      *hdfs.Client             // HDFS client used for metadata operations
	MetadataClientMutex sync.Mutex               // Serializing all metadata operations for simplicity (for now), TODO: allow N concurrent operations
	UserNameToUidCache  map[string]UidCacheEntry // cache for converting usernames to UIDs

	BaseDir string // mount on subdirectory, "" -> /
}

type UidCacheEntry struct {
	Uid     uint32    // User Id
	Expires time.Time // Absolute time when this cache entry expires
}

var _ HdfsAccessor = (*hdfsAccessorImpl)(nil) // ensure hdfsAccessorImpl implements HdfsAccessor

// Creates an instance of HdfsAccessor
func NewHdfsAccessor(nameNodeAddresses string, clock Clock, basedir string) (HdfsAccessor, error) {
	nns := strings.Split(nameNodeAddresses, ",")

	this := &hdfsAccessorImpl{
		NameNodeAddresses:  nns,
		Clock:              clock,
		UserNameToUidCache: make(map[string]UidCacheEntry),
		BaseDir:            basedir,
	}
	return this, nil
}

// Ensures that metadata client is connected
func (this *hdfsAccessorImpl) EnsureConnected() error {
	if this.MetadataClient != nil {
		return nil
	}
	return this.ConnectMetadataClient()
}

// Establishes connection to the name node (assigns MetadataClient field)
func (this *hdfsAccessorImpl) ConnectMetadataClient() error {
	client, err := this.ConnectToNameNode()
	if err != nil {
		return err
	}
	this.MetadataClient = client
	return nil
}

// Establishes connection to a name node in the context of some other operation
func (this *hdfsAccessorImpl) ConnectToNameNode() (*hdfs.Client, error) {
	// connecting to HDFS name node
	client, err := this.connectToNameNodeImpl()
	if err != nil {
		// Connection failed
		return nil, errors.New(fmt.Sprintf("Fail to connect to name node with error: %s", err.Error()))
	}
	Info.Println("Connected to name node")
	return client, nil
}

// Performs an attempt to connect to the HDFS name
func (this *hdfsAccessorImpl) connectToNameNodeImpl() (*hdfs.Client, error) {
	// Performing an attempt to connect to the name node
	// Colinmar's hdfs implementation has supported the multiple name node connection
	client, err := hdfs.NewClient(hdfs.ClientOptions{
		Addresses: this.NameNodeAddresses,
	})
	if err != nil {
		return nil, err
	}
	// connection is OK, but we need to check whether name node is operating ans expected
	// (this also checks whether name node is Active)
	// Performing this check, by doing Stat() for a path inside root directory
	// Note: The file '/$' doesn't have to be present
	// - both nil and ErrNotExists error codes indicate success of the operation
	_, statErr := client.Stat("/$")

	if pathError, ok := statErr.(*os.PathError); statErr == nil || ok && (pathError.Err == os.ErrNotExist) {
		// Succesfully connected
		return client, nil
	} else {
		client.Close()
		return nil, statErr
	}
}

// Opens HDFS file for reading
func (this *hdfsAccessorImpl) OpenRead(rpath string) (ReadSeekCloser, error) {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--OpenRead path chg  %s -> %s\n", rpath, path)

	// Blocking read. This is to reduce the connections pressue on hadoop-name-node
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return nil, err
		}
	}
	reader, err := this.MetadataClient.Open(path)
	if err != nil {
		return nil, err
	}
	return NewHdfsReader(reader), nil
}

// Creates new HDFS file
func (this *hdfsAccessorImpl) CreateFile(rpath string, mode os.FileMode) (HdfsWriter, error) {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--CreateFile path chg  %s -> %s\n", rpath, path)
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return nil, err
		}
	}
	writer, err := this.MetadataClient.CreateFile(path, 3, 64*1024*1024, mode)
	if err != nil {
		return nil, err
	}

	return NewHdfsWriter(writer), nil
}

func _chgPath(rpath, basedir string) string {
	if basedir == "" {
		return rpath
	}
	if rpath == "/" {
		return basedir
	}
	return basedir + rpath
}

// Enumerates HDFS directory
func (this *hdfsAccessorImpl) ReadDir(rpath string) ([]Attrs, error) {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--ReadDir path chg  %s -> %s\n", rpath, path)

	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return nil, err
		}
	}
	files, err := this.MetadataClient.ReadDir(path)
	if err != nil {
		if IsSuccessOrBenignError(err) {
			// benign error (e.g. path not found)
			return nil, err
		}
		// We've got error from this client, setting to nil, so we try another one next time
		this.MetadataClient = nil
		// TODO: attempt to gracefully close the conenction
		return nil, err
	}
	allAttrs := make([]Attrs, len(files))
	for i, fileInfo := range files {
		allAttrs[i] = this.AttrsFromFileInfo(fileInfo)
	}
	return allAttrs, nil
}

// Retrieves file/directory attributes
func (this *hdfsAccessorImpl) Stat(rpath string) (Attrs, error) {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--Stat path chg  %s -> %s\n", rpath, path)
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()

	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return Attrs{}, err
		}
	}

	fileInfo, err := this.MetadataClient.Stat(path)
	if err != nil {
		if IsSuccessOrBenignError(err) {
			// benign error (e.g. path not found)
			return Attrs{}, err
		}
		// We've got error from this client, setting to nil, so we try another one next time
		this.MetadataClient = nil
		// TODO: attempt to gracefully close the conenction
		return Attrs{}, err
	}
	return this.AttrsFromFileInfo(fileInfo), nil
}

// Retrieves HDFS usages
func (this *hdfsAccessorImpl) StatFs() (FsInfo, error) {
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()

	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return FsInfo{}, err
		}
	}

	fsInfo, err := this.MetadataClient.StatFs()
	if err != nil {
		if IsSuccessOrBenignError(err) {
			return FsInfo{}, err
		}
		this.MetadataClient = nil
		return FsInfo{}, err
	}
	return this.AttrsFromFsInfo(fsInfo), nil
}

// Converts os.FileInfo + underlying proto-buf data into Attrs structure
func (this *hdfsAccessorImpl) AttrsFromFileInfo(fileInfo os.FileInfo) Attrs {
	protoBufData := fileInfo.Sys().(*hadoop_hdfs.HdfsFileStatusProto)
	mode := os.FileMode(*protoBufData.Permission.Perm)
	if fileInfo.IsDir() {
		mode |= os.ModeDir
	}
	modificationTime := time.Unix(int64(protoBufData.GetModificationTime())/1000, 0)
	return Attrs{
		Inode:  *protoBufData.FileId,
		Name:   fileInfo.Name(),
		Mode:   mode,
		Size:   *protoBufData.Length,
		Uid:    this.LookupUid(*protoBufData.Owner),
		Mtime:  modificationTime,
		Ctime:  modificationTime,
		Crtime: modificationTime,
		Gid:    0} // TODO: Group is now hardcoded to be "root", implement proper mapping
}

func (this *hdfsAccessorImpl) AttrsFromFsInfo(fsInfo hdfs.FsInfo) FsInfo {
	return FsInfo{
		capacity:  fsInfo.Capacity,
		used:      fsInfo.Used,
		remaining: fsInfo.Remaining}
}

func HadoopTimestampToTime(timestamp uint64) time.Time {
	return time.Unix(int64(timestamp)/1000, 0)
}

// Performs a cache-assisted lookup of UID by username
func (this *hdfsAccessorImpl) LookupUid(userName string) uint32 {
	if userName == "" {
		return 0
	}
	// Note: this method is called under MetadataClientMutex, so accessing the cache dirctionary is safe
	cacheEntry, ok := this.UserNameToUidCache[userName]
	if ok && this.Clock.Now().Before(cacheEntry.Expires) {
		return cacheEntry.Uid
	}
	u, err := user.Lookup(userName)
	var uid64 uint64
	if err == nil {
		// UID is returned as string, need to parse it
		uid64, err = strconv.ParseUint(u.Uid, 10, 32)
	}
	if err != nil {
		uid64 = (1 << 31) - 1
	}
	this.UserNameToUidCache[userName] = UidCacheEntry{
		Uid:     uint32(uid64),
		Expires: this.Clock.Now().Add(5 * time.Minute)} // caching UID for 5 minutes
	return uint32(uid64)
}

// Returns true if err==nil or err is expected (benign) error which should be propagated directoy to the caller
func IsSuccessOrBenignError(err error) bool {
	if err == nil || err == io.EOF || err == fuse.EEXIST {
		return true
	}
	if pathError, ok := err.(*os.PathError); ok && (pathError.Err == os.ErrNotExist || pathError.Err == os.ErrPermission) {
		return true
	} else {
		return false
	}
}

// Creates a directory
func (this *hdfsAccessorImpl) Mkdir(rpath string, mode os.FileMode) error {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--Mkdir path chg  %s -> %s\n", rpath, path)

	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return err
		}
	}
	err := this.MetadataClient.Mkdir(path, mode)
	if err != nil {
		if strings.HasSuffix(err.Error(), "file already exists") {
			err = fuse.EEXIST
		}
	}
	return err
}

// Removes file or directory
func (this *hdfsAccessorImpl) Remove(rpath string) error {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--Remove path chg  %s -> %s\n", rpath, path)

	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return err
		}
	}
	return this.MetadataClient.Remove(path)
}

// Renames file or directory
func (this *hdfsAccessorImpl) Rename(roldPath string, rnewPath string) error {
	oldPath := _chgPath(roldPath, this.BaseDir)
	newPath := _chgPath(rnewPath, this.BaseDir)
	Info.Printf("--Rename path chg  %s -> %s\n", roldPath, oldPath)
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return err
		}
	}
	return this.MetadataClient.Rename(oldPath, newPath)
}

// Changes the mode of the file
func (this *hdfsAccessorImpl) Chmod(rpath string, mode os.FileMode) error {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--Chmod path chg  %s -> %s\n", rpath, path)

	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return err
		}
	}
	return this.MetadataClient.Chmod(path, mode)
}

// Changes the owner and group of the file
func (this *hdfsAccessorImpl) Chown(rpath string, user, group string) error {
	path := _chgPath(rpath, this.BaseDir)
	Info.Printf("--Remove path chg  %s -> %s\n", rpath, path)

	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()
	if this.MetadataClient == nil {
		if err := this.ConnectMetadataClient(); err != nil {
			return err
		}
	}
	return this.MetadataClient.Chown(path, user, group)
}

// Close current connection if needed
func (this *hdfsAccessorImpl) Close() error {
	this.MetadataClientMutex.Lock()
	defer this.MetadataClientMutex.Unlock()

	if this.MetadataClient != nil {
		err := this.MetadataClient.Close()
		this.MetadataClient = nil
		return err
	}
	return nil
}
