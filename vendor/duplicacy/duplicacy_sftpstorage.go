// Copyright (c) Acrosync LLC. All rights reserved.
// Free for personal use and commercial trial
// Commercial use requires per-user licenses available from https://duplicacy.com

package duplicacy

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"path"
	"runtime"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type SFTPStorage struct {
	RateLimitedStorage

	client          *sftp.Client
	storageDir      string
	numberOfThreads int
}

func CreateSFTPStorageWithPassword(server string, port int, username string, storageDir string,
	password string, threads int) (storage *SFTPStorage, err error) {

	authMethods := []ssh.AuthMethod{ssh.Password(password)}

	hostKeyCallback := func(hostname string, remote net.Addr,
		key ssh.PublicKey) error {
		return nil
	}

	return CreateSFTPStorage(server, port, username, storageDir, authMethods, hostKeyCallback, threads)
}

func CreateSFTPStorage(server string, port int, username string, storageDir string,
	authMethods []ssh.AuthMethod,
	hostKeyCallback func(hostname string, remote net.Addr,
		key ssh.PublicKey) error, threads int) (storage *SFTPStorage, err error) {

	config := &ssh.ClientConfig{
		User:            username,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
	}

	if server == "sftp.hidrive.strato.com" {
		config.Ciphers = []string{"aes128-cbc", "aes128-ctr", "aes256-ctr"}
	}

	serverAddress := fmt.Sprintf("%s:%d", server, port)
	connection, err := ssh.Dial("tcp", serverAddress, config)
	if err != nil {
		return nil, err
	}

	client, err := sftp.NewClient(connection)
	if err != nil {
		connection.Close()
		return nil, err
	}

	for storageDir[len(storageDir)-1] == '/' {
		storageDir = storageDir[:len(storageDir)-1]
	}

	fileInfo, err := client.Stat(storageDir)
	if err != nil {
		return nil, fmt.Errorf("Can't access the storage path %s: %v", storageDir, err)
	}

	if !fileInfo.IsDir() {
		return nil, fmt.Errorf("The storage path %s is not a directory", storageDir)
	}

	storage = &SFTPStorage{
		client:          client,
		storageDir:      storageDir,
		numberOfThreads: threads,
	}

	// Random number fo generating the temporary chunk file suffix.
	rand.Seed(time.Now().UnixNano())

	runtime.SetFinalizer(storage, CloseSFTPStorage)

	return storage, nil
}

func CloseSFTPStorage(storage *SFTPStorage) {
	storage.client.Close()
}

// ListFiles return the list of files and subdirectories under 'file' (non-recursively)
func (storage *SFTPStorage) ListFiles(threadIndex int, dirPath string) (files []string, sizes []int64, err error) {

	entries, err := storage.client.ReadDir(path.Join(storage.storageDir, dirPath))
	if err != nil {
		return nil, nil, err
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && name[len(name)-1] != '/' {
			name += "/"
		}

		files = append(files, name)
		sizes = append(sizes, entry.Size())
	}

	return files, sizes, nil
}

// DeleteFile deletes the file or directory at 'filePath'.
func (storage *SFTPStorage) DeleteFile(threadIndex int, filePath string) (err error) {
	fullPath := path.Join(storage.storageDir, filePath)
	fileInfo, err := storage.client.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			LOG_TRACE("SFTP_STORAGE", "File %s has disappeared before deletion", filePath)
			return nil
		}
		return err
	}
	if fileInfo == nil {
		return nil
	}
	return storage.client.Remove(path.Join(storage.storageDir, filePath))
}

// MoveFile renames the file.
func (storage *SFTPStorage) MoveFile(threadIndex int, from string, to string) (err error) {
	toPath := path.Join(storage.storageDir, to)
	fileInfo, err := storage.client.Stat(toPath)
	if fileInfo != nil {
		return fmt.Errorf("The destination file %s already exists", toPath)
	}
	return storage.client.Rename(path.Join(storage.storageDir, from),
		path.Join(storage.storageDir, to))
}

// CreateDirectory creates a new directory.
func (storage *SFTPStorage) CreateDirectory(threadIndex int, dirPath string) (err error) {
	fullPath := path.Join(storage.storageDir, dirPath)
	fileInfo, err := storage.client.Stat(fullPath)
	if fileInfo != nil && fileInfo.IsDir() {
		return nil
	}
	return storage.client.Mkdir(path.Join(storage.storageDir, dirPath))
}

// GetFileInfo returns the information about the file or directory at 'filePath'.
func (storage *SFTPStorage) GetFileInfo(threadIndex int, filePath string) (exist bool, isDir bool, size int64, err error) {
	fileInfo, err := storage.client.Stat(path.Join(storage.storageDir, filePath))
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, 0, nil
		} else {
			return false, false, 0, err
		}
	}

	if fileInfo == nil {
		return false, false, 0, nil
	}

	return true, fileInfo.IsDir(), fileInfo.Size(), nil
}

// FindChunk finds the chunk with the specified id.  If 'isFossil' is true, it will search for chunk files with
// the suffix '.fsl'.
func (storage *SFTPStorage) FindChunk(threadIndex int, chunkID string, isFossil bool) (filePath string, exist bool, size int64, err error) {
	dir := path.Join(storage.storageDir, "chunks")

	suffix := ""
	if isFossil {
		suffix = ".fsl"
	}

	// The minimum level of directories to dive into before searching for the chunk file.
	minimumLevel := 2

	for level := 0; level*2 < len(chunkID); level++ {
		if level >= minimumLevel {
			filePath = path.Join(dir, chunkID[2*level:]) + suffix
			if stat, err := storage.client.Stat(filePath); err == nil && !stat.IsDir() {
				return filePath[len(storage.storageDir)+1:], true, stat.Size(), nil
			} else if err == nil && stat.IsDir() {
				return filePath[len(storage.storageDir)+1:], true, 0, fmt.Errorf("The path %s is a directory", filePath)
			}
		}

		// Find the subdirectory the chunk file may reside.
		subDir := path.Join(dir, chunkID[2*level:2*level+2])
		stat, err := storage.client.Stat(subDir)
		if err == nil && stat.IsDir() {
			dir = subDir
			continue
		}

		if level < minimumLevel {
			// Create the subdirectory if is doesn't exist.

			if err == nil && !stat.IsDir() {
				return "", false, 0, fmt.Errorf("The path %s is not a directory", subDir)
			}

			err = storage.client.Mkdir(subDir)
			if err != nil {
				// The directory may have been created by other threads so check it again.
				stat, _ := storage.client.Stat(subDir)
				if stat == nil || !stat.IsDir() {
					return "", false, 0, fmt.Errorf("Failed to create the directory %s: %v", subDir, err)
				}
			}

			dir = subDir
			continue
		}

		// Teh chunk must be under this subdirectory but it doesn't exist.
		return path.Join(dir, chunkID[2*level:])[len(storage.storageDir)+1:] + suffix, false, 0, nil

	}

	LOG_FATAL("CHUNK_FIND", "Chunk %s is still not found after having searched a maximum level of directories",
		chunkID)
	return "", false, 0, nil
}

// DownloadFile reads the file at 'filePath' into the chunk.
func (storage *SFTPStorage) DownloadFile(threadIndex int, filePath string, chunk *Chunk) (err error) {
	file, err := storage.client.Open(path.Join(storage.storageDir, filePath))

	if err != nil {
		return err
	}

	defer file.Close()
	if _, err = RateLimitedCopy(chunk, file, storage.DownloadRateLimit/storage.numberOfThreads); err != nil {
		return err
	}

	return nil
}

// UploadFile writes 'content' to the file at 'filePath'.
func (storage *SFTPStorage) UploadFile(threadIndex int, filePath string, content []byte) (err error) {

	fullPath := path.Join(storage.storageDir, filePath)

	letters := "abcdefghijklmnopqrstuvwxyz"
	suffix := make([]byte, 8)
	for i := range suffix {
		suffix[i] = letters[rand.Intn(len(letters))]
	}

	temporaryFile := fullPath + "." + string(suffix) + ".tmp"

	file, err := storage.client.OpenFile(temporaryFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}

	reader := CreateRateLimitedReader(content, storage.UploadRateLimit/storage.numberOfThreads)
	_, err = io.Copy(file, reader)
	if err != nil {
		file.Close()
		return err
	}
	file.Close()

	err = storage.client.Rename(temporaryFile, fullPath)
	if err != nil {

		if _, err = storage.client.Stat(fullPath); err == nil {
			storage.client.Remove(temporaryFile)
			return nil
		} else {
			return fmt.Errorf("Uploaded file but failed to store it at %s: %v", fullPath, err)
		}
	}

	return nil
}

// If a local snapshot cache is needed for the storage to avoid downloading/uploading chunks too often when
// managing snapshots.
func (storage *SFTPStorage) IsCacheNeeded() bool { return true }

// If the 'MoveFile' method is implemented.
func (storage *SFTPStorage) IsMoveFileImplemented() bool { return true }

// If the storage can guarantee strong consistency.
func (storage *SFTPStorage) IsStrongConsistent() bool { return true }

// If the storage supports fast listing of files names.
func (storage *SFTPStorage) IsFastListing() bool { return false }

// Enable the test mode.
func (storage *SFTPStorage) EnableTestMode() {}
