package store

import (
	"crypto/sha256"
	"io"
	"os"

	"github.com/openclaw/wacrawl/internal/mediafile"
)

// mediaRetention holds the roots whose media an import keeps when the new
// source lacks it: --copy-media's directory, and earlier --adopt-source roots.
type mediaRetention struct {
	copied   string
	archived []string
}

// retainArchivedSourceMedia reads no file: an archived root may be cloud
// storage, where opening a file downloads it.
func retainArchivedSourceMedia(roots []string, old, incoming Message) Message {
	// Compare raw types too: some (round video notes) have no media type.
	if incoming.MediaPath != "" || old.MediaPath == "" || old.RawType != incoming.RawType || old.MediaType != incoming.MediaType {
		return incoming
	}
	for _, root := range roots {
		if mediafile.Within(root, old.MediaPath) {
			incoming.MediaPath = old.MediaPath
			return incoming
		}
	}
	return incoming
}

func sameAttachment(a, b Message) bool {
	return a.MediaType != "" && (a.MediaURL != "" || a.MediaSize > 0) &&
		a.RawType == b.RawType && a.MediaType == b.MediaType &&
		a.MediaURL == b.MediaURL && a.MediaSize == b.MediaSize && a.MediaTitle == b.MediaTitle
}

// The caller has already validated source/account/event identity and computed
// payload-cleared tombstones from the original incoming observation.
func retainArchivedMedia(root string, old, incoming Message) (Message, error) {
	if !sameAttachment(old, incoming) || old.MediaPath == "" || !mediafile.Within(root, old.MediaPath) {
		return incoming, nil
	}
	previous, err := mediafile.Open(root, old.MediaPath)
	if os.IsNotExist(err) {
		return incoming, nil
	}
	if err != nil {
		return incoming, err
	}
	defer func() { _ = previous.Close() }()
	if incoming.MediaPath != "" && mediafile.Within(root, incoming.MediaPath) {
		current, err := mediafile.Open(root, incoming.MediaPath)
		if err != nil && !os.IsNotExist(err) {
			return incoming, err
		}
		if err == nil {
			defer func() { _ = current.Close() }()
			left, right := sha256.New(), sha256.New()
			if _, err := io.Copy(left, previous); err != nil {
				return incoming, err
			}
			if _, err := io.Copy(right, current); err != nil {
				return incoming, err
			}
			if string(left.Sum(nil)) != string(right.Sum(nil)) {
				incoming.mediaContentChanged = true
				return incoming, nil
			}
		}
	}
	incoming.MediaPath = old.MediaPath
	return incoming, nil
}
