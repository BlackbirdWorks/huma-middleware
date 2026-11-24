package compression

import "strings"

// eventStream is the media type of server-sent events.
const eventStream = "text/event-stream"

// mediaType returns the lower-case media type of a Content-Type value, without
// parameters.
func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")

	return strings.ToLower(strings.TrimSpace(mt))
}

// matchType reports whether the media type mt matches pattern: "*/*" (any),
// "type/*" (a whole type), "+suffix" (a structured syntax suffix such as
// "+json") or an exact "type/subtype". Patterns are lower case.
//
// A stream of server-sent events is only matched by its exact name: no wildcard
// covers "text/event-stream", as events must reach the client as they are written.
func matchType(pattern, mt string) bool {
	switch {
	case mt == eventStream:
		return pattern == mt
	case pattern == "*/*":
		return true
	case strings.HasPrefix(pattern, "+"):
		return strings.HasSuffix(mt, pattern)
	case strings.HasSuffix(pattern, "/*"):
		return strings.HasPrefix(mt, strings.TrimSuffix(pattern, "*"))
	default:
		return mt == pattern
	}
}

// alreadyCompressed reports whether the media type is a format that is
// compressed already, whatever the allow-list says.
func alreadyCompressed(mt string) bool {
	switch mt {
	case "image/svg+xml", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return false
	case "application/zip", "application/gzip", "application/x-gzip", "application/zstd",
		"application/x-bzip2", "application/x-xz", "application/x-7z-compressed",
		"application/x-rar-compressed", "application/vnd.rar", "application/java-archive",
		"application/pdf", "application/x-brotli", "application/x-lzma":
		return true
	}

	return strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "video/") ||
		strings.HasPrefix(mt, "audio/") || mt == "font/woff" || mt == "font/woff2"
}
