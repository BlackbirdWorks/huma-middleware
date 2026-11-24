package compression

import "strings"

// mediaType returns the lower-case media type of a Content-Type value, without
// parameters.
func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")

	return strings.ToLower(strings.TrimSpace(mt))
}

// matchType reports whether the media type mt matches pattern: "*/*" (any),
// "type/*" (a whole type), "+suffix" (a structured syntax suffix such as
// "+json") or an exact "type/subtype". Patterns are lower case.
func matchType(pattern, mt string) bool {
	switch {
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

// defaultTypes are compressed unless the allow-list is replaced with
// [WithContentTypes].
func defaultTypes() []string {
	return []string{
		"text/*", "+json", "+xml", "application/json", "application/javascript", "application/x-javascript",
		"application/ecmascript", "application/xml", "application/x-ndjson", "application/yaml",
		"application/x-yaml", "application/graphql", "application/wasm",
	}
}

// isDefaultAllowed matches the default list, minus Server-Sent Events, which
// must reach the client as they are written.
func isDefaultAllowed(mt string) bool {
	if mt == "text/event-stream" {
		return false
	}
	for _, p := range defaultTypes() {
		if matchType(p, mt) {
			return true
		}
	}

	return false
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
