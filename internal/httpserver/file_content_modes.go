package httpserver

import "net/http"

// Method dispatch reflects the immutable assembly without wrapping the writer.
func HandlerWithFileContentModes(next http.Handler, uploadEnabled, businessEnabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, matched := downloadContentPath(r); !matched {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodGet {
			if !businessEnabled {
				writeAdminError(w, 503, "file_download_unavailable")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPut && uploadEnabled {
			next.ServeHTTP(w, r)
			return
		}
		allow := "GET"
		if uploadEnabled {
			allow += ", PUT"
		}
		w.Header().Set("Allow", allow)
		writeAdminError(w, 405, "method_not_allowed")
	})
}
