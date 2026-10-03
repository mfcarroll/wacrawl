package store

import "testing"

func TestRetainArchivedSourceMedia(t *testing.T) {
	roots := []string{"/archive/phone"}
	kept := "/archive/phone/Message/Media/x/a.mp4"
	for _, tc := range []struct {
		name     string
		old, in  Message
		wantPath string
	}{
		{"photo the new source never downloaded", Message{RawType: 1, MediaType: "image", MediaPath: "/archive/phone/a.jpg"}, Message{RawType: 1, MediaType: "image"}, "/archive/phone/a.jpg"},
		{"type with no media type, e.g. a video note", Message{RawType: 23, MediaPath: kept}, Message{RawType: 23}, kept},
		{"new source has its own file", Message{RawType: 1, MediaType: "image", MediaPath: "/archive/phone/a.jpg"}, Message{RawType: 1, MediaType: "image", MediaPath: "/desktop/a.jpg"}, "/desktop/a.jpg"},
		{"different message type", Message{RawType: 1, MediaType: "image", MediaPath: "/archive/phone/a.jpg"}, Message{RawType: 14}, ""},
		{"different media type", Message{RawType: 1, MediaType: "image", MediaPath: "/archive/phone/a.jpg"}, Message{RawType: 1, MediaType: "video"}, ""},
		{"old path outside archived roots", Message{RawType: 1, MediaType: "image", MediaPath: "/elsewhere/a.jpg"}, Message{RawType: 1, MediaType: "image"}, ""},
	} {
		if got := retainArchivedSourceMedia(roots, tc.old, tc.in).MediaPath; got != tc.wantPath {
			t.Errorf("%s: media path = %q, want %q", tc.name, got, tc.wantPath)
		}
	}
}
