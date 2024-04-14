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
	"io"
	"net/http"
)

type hijackedReadCloser struct {
	originalReadCloser io.ReadCloser
}

func (h *hijackedReadCloser) Read(p []byte) (int, error) {
	// gofail: var DemoDropRequestBodyFailPoint struct{}
	// return discardReadData(h.originalReadCloser, p)

	if h.originalReadCloser == nil {
		return 0, nil
	}
	return h.originalReadCloser.Read(p)
}

func (h *hijackedReadCloser) Close() error {
	if h.originalReadCloser == nil {
		return nil
	}
	return h.originalReadCloser.Close()
}

/* helper functions */
func hijackRequestBody(r *http.Request) {
	r.Body = &hijackedReadCloser{
		originalReadCloser: r.Body,
	}
}

func discardReadData(rc io.ReadCloser, p []byte) (int, error) {
	// return rc.Read(make([]byte, len(p)))

	_, err := rc.Read(make([]byte, len(p)))
	return 0, err // discard data but return original error

	// return 0, nil
}
