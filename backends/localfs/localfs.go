package localfs

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"time"
	"encoding/hex"

	"github.com/andreimarcu/linx-server/backends"
	"github.com/andreimarcu/linx-server/helpers"
	"github.com/andreimarcu/linx-server/expiry"
	"github.com/minio/sha256-simd"
	"github.com/gabriel-vasile/mimetype"
)

type LocalfsBackend struct {
	metaPath  string
	filesPath string
}

type MetadataJSON struct {
	DeleteKey    string   `json:"delete_key"`
	AccessKey    string   `json:"access_key,omitempty"`
	Sha256sum    string   `json:"sha256sum"`
	Mimetype     string   `json:"mimetype"`
	Size         int64    `json:"size"`
	Expiry       int64    `json:"expiry"`
	SrcIp        string   `json:"srcip,omitempty"`
  OriginalName string   `json:"original_name,omitempty"`
	ArchiveFiles []string `json:"archive_files,omitempty"`
}

func (b LocalfsBackend) Delete(key string) (err error) {
	err = os.Remove(path.Join(b.filesPath, key))
	if err != nil {
		return
	}
	err = os.Remove(path.Join(b.metaPath, key))
	return
}

func (b LocalfsBackend) Exists(key string) (bool, error) {
	_, err := os.Stat(path.Join(b.filesPath, key))
	return err == nil, err
}

func (b LocalfsBackend) Head(key string) (metadata backends.Metadata, err error) {
	f, err := os.Open(path.Join(b.metaPath, key))
	if os.IsNotExist(err) {
		return metadata, backends.NotFoundErr
	} else if err != nil {
		return metadata, backends.BadMetadata
	}
	defer f.Close()

	decoder := json.NewDecoder(f)

	mjson := MetadataJSON{}
	if err := decoder.Decode(&mjson); err != nil {
		return metadata, backends.BadMetadata
	}

	metadata.DeleteKey = mjson.DeleteKey
	metadata.AccessKey = mjson.AccessKey
	metadata.Mimetype = mjson.Mimetype
	metadata.ArchiveFiles = mjson.ArchiveFiles
	metadata.OriginalName = mjson.OriginalName
	metadata.Sha256sum = mjson.Sha256sum
	metadata.Expiry = time.Unix(mjson.Expiry, 0)
	metadata.Size = mjson.Size

	return
}

func (b LocalfsBackend) Get(key string) (metadata backends.Metadata, f io.ReadCloser, err error) {
	metadata, err = b.Head(key)
	if err != nil {
		return
	}

	f, err = os.Open(path.Join(b.filesPath, key))
	if err != nil {
		return
	}

	return
}

func (b LocalfsBackend) ServeFile(key string, w http.ResponseWriter, r *http.Request) (err error) {
	_, err = b.Head(key)
	if err != nil {
		return
	}

	filePath := path.Join(b.filesPath, key)
	http.ServeFile(w, r, filePath)

	return
}

func (b LocalfsBackend) writeMetadata(key string, metadata backends.Metadata) error {
	metaPath := path.Join(b.metaPath, key)

	mjson := MetadataJSON{
		DeleteKey:    metadata.DeleteKey,
		AccessKey:    metadata.AccessKey,
		Mimetype:     metadata.Mimetype,
		ArchiveFiles: metadata.ArchiveFiles,
		OriginalName: metadata.OriginalName,
		Sha256sum:    metadata.Sha256sum,
		Expiry:       metadata.Expiry.Unix(),
		Size:         metadata.Size,
		SrcIp:        metadata.SrcIp,
		
	}

	dst, err := os.Create(metaPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	encoder := json.NewEncoder(dst)
	err = encoder.Encode(mjson)
	if err != nil {
		os.Remove(metaPath)
		return err
	}

	return nil
}

func (b LocalfsBackend) Put(key string, r io.Reader, expiryTime time.Duration, deleteKey, accessKey string, srcIp string, originalName string) (m backends.Metadata, err error) {
	filePath := path.Join(b.filesPath, key)

	hasher := sha256.New()
	dst, err := os.Create(filePath)
	if err != nil {
		return
	}
	defer dst.Close()

	bytes, err := io.Copy(dst, io.TeeReader(r, hasher))
	if bytes == 0 {
		os.Remove(filePath)
		return m, backends.FileEmptyError
	} else if err != nil {
		os.Remove(filePath)
		return m, err
	} else if bytes >= backends.Limits.MaxSize {
		os.Remove(filePath)
		return m, backends.FileTooLargeError
	}

	var fileExpiry time.Time
	maxDurationTime := time.Duration(backends.Limits.MaxDurationTime) * time.Second
	if expiryTime == 0 {
		if bytes > backends.Limits.MaxDurationSize && maxDurationTime > 0 {
			fileExpiry = time.Now().Add(maxDurationTime)
		} else {
			fileExpiry = expiry.NeverExpire
		}
	} else {
		if bytes > backends.Limits.MaxDurationSize && expiryTime > maxDurationTime {
			fileExpiry = time.Now().Add(maxDurationTime)
		} else {
			fileExpiry = time.Now().Add(expiryTime)
		}
	}

	dst.Seek(0, 0)
	// Get first 512 bytes for mimetype detection
	header := make([]byte, 512)
	headerlen, err := dst.Read(header)
	if err != nil {
		os.Remove(filePath)
		return
	}
	// Use the bytes we extracted earlier and attempt to determine the file
	// type
	kind := mimetype.Detect(header[:headerlen])
	m.Mimetype = kind.String()

	dst.Seek(0, 0)

  m.Size = bytes
  m.Sha256sum = hex.EncodeToString(hasher.Sum(nil))
	m.Expiry = fileExpiry
	m.DeleteKey = deleteKey
	m.AccessKey = accessKey
	m.SrcIp = srcIp
	m.ArchiveFiles, _ = helpers.ListArchiveFiles(m.Mimetype, m.Size, dst)
	m.OriginalName = originalName

	err = b.writeMetadata(key, m)
	if err != nil {
		os.Remove(filePath)
		return
	}

	return
}

func (b LocalfsBackend) PutMetadata(key string, m backends.Metadata) (err error) {
	err = b.writeMetadata(key, m)
	if err != nil {
		return
	}

	return
}

func (b LocalfsBackend) Size(key string) (int64, error) {
	fileInfo, err := os.Stat(path.Join(b.filesPath, key))
	if err != nil {
		return 0, err
	}

	return fileInfo.Size(), nil
}

func (b LocalfsBackend) List() ([]string, error) {
	var output []string

	files, err := os.ReadDir(b.filesPath)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		output = append(output, file.Name())
	}

	return output, nil
}

func NewLocalfsBackend(metaPath string, filesPath string) LocalfsBackend {
	return LocalfsBackend{
		metaPath:  metaPath,
		filesPath: filesPath,
	}
}
