// Copyright 2024 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rafthttp

import (
	"net/http"
)

type hijackedResponseWriter struct {
	originalResponseWriter http.ResponseWriter
}

func (h *hijackedResponseWriter) Header() http.Header {
	return h.originalResponseWriter.Header()
}

func (h *hijackedResponseWriter) Write(p []byte) (int, error) {
	// When hijacking, we drop the data to be written completely
	// gofail: var HijackResponseWriterFailPoint struct{}
	// return discardWriteData(p)

	if h.originalResponseWriter == nil {
		return 0, nil
	}
	return h.originalResponseWriter.Write(p)
}

func (h *hijackedResponseWriter) WriteHeader(statusCode int) {
	// When hijacking, we drop the data to be written completely
	// gofail: var HijackResponseWriterHeaderFailPoint struct{}
	// return

	if h.originalResponseWriter == nil {
		return
	}
	h.originalResponseWriter.WriteHeader(statusCode)
}

func (h *hijackedResponseWriter) Flush() {
	h.originalResponseWriter.(http.Flusher).Flush()
}

/* helper functions */
func hijackResponseWriter(w http.ResponseWriter) *hijackedResponseWriter {
	return &hijackedResponseWriter{
		originalResponseWriter: w,
	}
}

func discardWriteData(p []byte) (int, error) {
	return 0, nil
}
