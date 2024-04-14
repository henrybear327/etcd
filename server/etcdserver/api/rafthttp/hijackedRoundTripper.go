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

import "net/http"

/* for stream */
type hijackedStreamRoundTripper struct {
	// in order to preserve the already configured Transport for pipeline and stream
	http.Transport
}

func (t *hijackedStreamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	hijackRequestBody(r)
	return t.Transport.RoundTrip(r)
}

/* for pipeline */

type hijackedPipelineRoundTripper struct {
	http.Transport
}

func (t *hijackedPipelineRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	hijackRequestBody(r)
	return t.Transport.RoundTrip(r)
}
