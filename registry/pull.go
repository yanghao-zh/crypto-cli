// Copyright © 2018 SENETAS SECURITY PTY LTD
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

package registry

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/api/v2"
	dauth "github.com/docker/distribution/registry/client/auth"
	"github.com/docker/docker/registry"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/Senetas/crypto-cli/crypto"
	"github.com/Senetas/crypto-cli/distribution"
	"github.com/Senetas/crypto-cli/registry/auth"
	"github.com/Senetas/crypto-cli/registry/httpclient"
	"github.com/Senetas/crypto-cli/registry/names"
	"github.com/Senetas/crypto-cli/utils"
	pb "gopkg.in/cheggaaa/pb.v1"
)

// PullImage pulls an image from a remote repository
func PullImage(
	token dauth.Scope,
	ref names.NamedTaggedRepository,
	endpoint *registry.APIEndpoint,
	opts *crypto.Opts,
	downloadDir string,
) (*distribution.ImageManifest, error) {
	bldr := v2.NewURLBuilder(endpoint.URL, false)

	manifest, err := PullManifest(token, ref, bldr, downloadDir)
	if err != nil {
		return nil, err
	}
	log.Info().Msg("Mainfest obtained.")

	if err = manifest.DecryptKeys(opts, ref); err != nil {
		return nil, err
	}

	log.Info().Msgf("Downloading config: %s.", manifest.Config.GetDigest())
	filename, err := PullFromDigest(
		token,
		ref,
		manifest.Config.GetDigest(),
		bldr,
		downloadDir,
	)
	if err != nil {
		return nil, err
	}
	manifest.Config.SetFilename(filename)

	log.Info().Msg("Downloading layers:")
	for _, l := range manifest.Layers {
		log.Info().Msgf("Downloading: %s.", l.GetDigest())
		filename, err := PullFromDigest(
			token,
			ref,
			l.GetDigest(),
			bldr,
			downloadDir,
		)
		if err != nil {
			return nil, err
		}
		l.SetFilename(filename)
	}

	return manifest, nil
}

// PullManifest pulls a manifest from the registry and parses it
func PullManifest(
	token dauth.Scope,
	ref reference.Named,
	bldr *v2.URLBuilder,
	dir string,
) (_ *distribution.ImageManifest, err error) {
	urlStr, err := bldr.BuildManifestURL(ref)
	if err != nil {
		return nil, errors.Wrapf(err, "ref = %v", ref)
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "GET %s", urlStr)
	}

	// TODO: Handle list manifests
	req.Header.Set("Accept", distribution.MediaTypeManifest)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	auth.AddToReqest(token, req)

	resp, err := httpclient.DoRequest(httpclient.DefaultClient, req, true, true)
	if resp != nil {
		defer func() { err = utils.CheckedClose(resp.Body, err) }()
	}
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("manifest download failed with status: " + resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	manifest := &distribution.ImageManifest{DirName: dir}
	if err = json.Unmarshal(body, manifest); err != nil {
		return nil, errors.WithStack(err)
	}

	log.Debug().Msg(spew.Sdump(manifest))

	return manifest, nil
}

// PullFromDigest downloads a blob (refereced by its digest) from the registry to a temporay file.
// It verifies that the downloaded matches its digest, deleting if if it does not
func PullFromDigest(
	token dauth.Scope,
	ref reference.Named,
	d digest.Digest,
	bldr *v2.URLBuilder,
	dir string,
) (fn string, err error) {
	sep := names.SeperateRepository(ref)
	can := names.AppendDigest(sep, d)
	fn = filepath.Join(dir, d.Encoded())

	urlStr, err := bldr.BuildBlobURL(can)
	if err != nil {
		return "", errors.Wrapf(err, "%#v", ref)
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return "", errors.Wrapf(err, "GET %s", urlStr)
	}

	req.Header.Set("Accept", distribution.MediaTypeLayer)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	auth.AddToReqest(token, req)

	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	nameCh := make(chan string)
	defer close(nameCh)
	errChan := make(chan error)
	defer close(errChan)

	// timeout
	timer := time.AfterFunc(10*time.Second, cancel)

	go download(ctx, req, timer, dir, fn, d, errChan)

	err = <-errChan
	return
}

func quitUnVerified(fn string, fh io.Closer, err error) error {
	if err2 := os.Remove(fn); err != nil {
		return errors.Wrapf(
			utils.Errors{err, err2},
			"unverified data was NOT deleted. To clean manaually delete: %s",
			fn,
		)
	}

	if err2 := fh.Close(); err2 != nil {
		return errors.Wrap(
			utils.Errors{err, err2},
			"digest verification failed, failed to close, but unverified data was deleted",
		)
	}

	return errors.Wrapf(err, "digest verification failed, unverified data deleted")
}

func download(
	ctx context.Context,
	req *http.Request,
	timer *time.Timer,
	dir, fn string,
	d digest.Digest,
	errChan chan<- error,
) {
	resp, err := httpclient.DoRequest(&http.Client{}, req, true, false)
	if resp != nil {
		defer func() { err = utils.CheckedClose(resp.Body, err) }()
	}
	if err != nil {
		errChan <- err
		return
	}

	select {
	case <-ctx.Done():
		errChan <- errors.New("request timed out")
		return
	default:
	}

	if resp.StatusCode != http.StatusOK {
		errChan <- errors.Errorf("Failed to download blob %s", fn)
		return
	}

	fh, err := os.Create(fn)
	if err != nil {
		errChan <- errors.Wrapf(err, "filename = %s", fn)
		return
	}
	defer func() { err = utils.CheckedClose(fh, err) }()

	errChan <- respond(resp, d, fn, fh, timer)
}

func respond(
	resp *http.Response,
	d digest.Digest,
	fn string,
	fh io.WriteCloser,
	timer *time.Timer,
) (err error) {
	size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
	if err != nil {
		err = errors.WithStack(err)
		return
	}

	bar := pb.New(size).SetUnits(pb.U_BYTES)
	vw := d.Verifier()
	mw := io.MultiWriter(vw, fh, bar)

	bar.Start()

	// reset timeout evertime there is new data
	for {
		timer.Reset(2 * time.Second)
		_, err = io.CopyN(mw, resp.Body, 512)
		if err == io.EOF {
			break
		} else if err != nil {
			err = errors.Wrapf(err, "filename = %s", fn)
			return
		}
	}

	bar.Finish()

	if !vw.Verified() {
		return quitUnVerified(fn, fh, err)
	}

	return nil
}
