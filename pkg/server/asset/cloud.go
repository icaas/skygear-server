// Copyright 2015-present Oursky Ltd.
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

package asset

import (
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/franela/goreq"
)

const (
	cloudAssetSignerTokenRefreshInterval = 30 * time.Minute
	cloudAssetSignerTokenExpiryInterval  = 2 * time.Hour
)

// CloudStore models the skygear cloud asset store
type CloudStore struct {
	appName   string
	host      string
	authToken string
	urlPrefix string
	public    bool

	// signer related
	signerToken              string
	signerExtra              string
	signerTokenExpiredAt     time.Time
	signerTokenRefreshTicker *time.Ticker
	signerTokenMutex         *sync.RWMutex
}

type refreshSignerTokenResponse struct {
	Value     string    `json:"value"`
	Extra     string    `json:"extra"`
	ExpiredAt time.Time `json:"expired_at"`
}

// NewCloudStore creates a new cloud asset store
func NewCloudStore(
	appName string,
	host string,
	authToken string,
	publicURLPrefix string,
	privateURLPrefix string,
	public bool,
) (*CloudStore, error) {
	if appName == "" {
		return nil, errors.New("Missing app name for cloud asset")
	}

	if host == "" {
		return nil, errors.New("Missing host for cloud asset")
	}

	if authToken == "" {
		return nil, errors.New("Missing auth token for cloud asset")
	}

	if public && publicURLPrefix == "" {
		return nil, errors.New("Missing public URL prefix for cloud asset")
	}

	if !public && privateURLPrefix == "" {
		return nil, errors.New("Missing private URL prefix for cloud asset")
	}

	urlPrefix := privateURLPrefix
	if public {
		urlPrefix = publicURLPrefix
	}

	store := &CloudStore{
		appName:   appName,
		host:      host,
		authToken: authToken,
		public:    public,
		urlPrefix: urlPrefix,
	}

	log.
		WithField("cloud-store", store).
		Info("Created Cloud Asset Store")

	// setup ticker to refresh signer token
	store.signerTokenMutex = &sync.RWMutex{}
	store.signerTokenRefreshTicker = time.NewTicker(
		cloudAssetSignerTokenRefreshInterval,
	)
	go func(s *CloudStore) {
		for tickerTime := range s.signerTokenRefreshTicker.C {
			log.
				WithField("time", tickerTime).
				Info("Cloud Asset Signer Token Refresh Ticker Trigger")

			s.refreshSignerToken()
		}
	}(store)
	go store.refreshSignerToken()

	return store, nil
}

func (s *CloudStore) refreshSignerToken() {
	log.Info("Start refresh Cloud Asset Signer Token")

	urlString := strings.Join(
		[]string{s.host, "token", s.appName},
		"/",
	)
	expiredAt := time.Now().
		Add(cloudAssetSignerTokenExpiryInterval).
		Unix()

	req := goreq.Request{
		Uri:     urlString,
		Timeout: 10 * time.Second,
		QueryString: struct {
			ExpiredAt int64 `url:"expired_at"`
		}{expiredAt},
	}.WithHeader("Authorization", "Bearer "+s.authToken)

	res, err := req.Do()
	if err != nil {
		log.
			WithField("url", urlString).
			WithField("expired-at", expiredAt).
			WithField("error", err).
			Error("Fail to request to refresh Cloud Asset Signer Token")

		return
	}

	resBody := refreshSignerTokenResponse{}
	err = res.Body.FromJsonTo(&resBody)
	if err != nil {
		log.
			WithField("error", err).
			WithField("response", res.Body).
			Error("Fail to parse the response for refresh Cloud Asset Signer Token")

		return
	}

	log.
		WithField("response", resBody).
		Info("Successfully got new Cloud Asset Signer Token")

	s.signerTokenMutex.Lock()
	s.signerToken = resBody.Value
	s.signerExtra = resBody.Extra
	s.signerTokenExpiredAt = resBody.ExpiredAt
	s.signerTokenMutex.Unlock()
}

// GetFileReader returns a reader for reading files
func (s CloudStore) GetFileReader(name string) (io.ReadCloser, error) {
	return nil, errors.New(
		"Directly getting files is not available for cloud-based asset store",
	)
}

// PutFileReader return a writer for uploading files
func (s CloudStore) PutFileReader(
	name string,
	src io.Reader,
	length int64,
	contentType string,
) error {
	return errors.New(
		"Directly uploading files is not available for cloud-based asset store",
	)
}

// SignedURL return a signed URL with expiry date
func (s CloudStore) SignedURL(name string) (string, error) {
	// TODO: Generate signed URL
	return strings.Join(
		[]string{
			s.urlPrefix,
			s.appName,
			name,
		},
		"/",
	), nil
}

// IsSignatureRequired indicates whether a signature is required
func (s CloudStore) IsSignatureRequired() bool {
	return !s.public
}

// ParseSignature tries to parse the asset signature
func (s CloudStore) ParseSignature(
	signed string,
	name string,
	expiredAt time.Time,
) (bool, error) {

	return false, errors.New(
		"Asset signature parsing for cloud-based asset store is not available",
	)
}
