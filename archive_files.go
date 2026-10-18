package main

import (
	"errors"

	sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"
	"github.com/aiii-dot-id/aiios-planning-plugin/internal/t3"
)

type privateArchive struct{}

func missingFile(err error) bool {
	var op *sdk.OperationError
	return errors.As(err, &op) && op.ReasonCode == "FS_NOT_FOUND"
}
func (privateArchive) Read(path string) ([]byte, bool, error) {
	var out []byte
	var size int64 = -1
	for {
		data, eof, total, err := sdk.Files.Read(sdk.PrivateRoot, path, int64(len(out)), 0)
		if err != nil {
			if missingFile(err) && len(out) == 0 {
				return nil, false, nil
			}
			return nil, false, err
		}
		if total < 0 || total > t3.MaxRecordBytes || (size >= 0 && size != total) {
			return nil, false, t3.ErrArchive
		}
		size = total
		out = append(out, data...)
		if len(out) > t3.MaxRecordBytes || int64(len(out)) > size {
			return nil, false, t3.ErrArchive
		}
		if eof {
			if int64(len(out)) != size {
				return nil, false, t3.ErrArchive
			}
			return out, true, nil
		}
		if len(data) == 0 {
			return nil, false, t3.ErrArchive
		}
	}
}
func (privateArchive) Publish(path string, data []byte, expected string) (t3.Publication, error) {
	var proof sdk.PublishResult
	var err error
	if expected == "" {
		proof, err = sdk.Files.PublishNew(sdk.PrivateRoot, path, data)
	} else {
		proof, err = sdk.Files.Publish(sdk.PrivateRoot, path, data, expected)
	}
	return t3.Publication{Size: proof.Size, SHA256: proof.SHA256, Durable: proof.Durable}, err
}
func (privateArchive) List(path string) ([]t3.ArchiveEntry, bool, error) {
	entries, more, err := sdk.Files.List(sdk.PrivateRoot, path)
	if missingFile(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	result := make([]t3.ArchiveEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, t3.ArchiveEntry{Name: e.Name, Dir: e.Dir, Symlink: e.Symlink})
	}
	return result, more, nil
}
