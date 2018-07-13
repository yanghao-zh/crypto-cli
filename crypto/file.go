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

package crypto

import (
	"crypto/rand"
	"io"
	"os"

	"github.com/minio/sio"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"

	"github.com/Senetas/crypto-cli/utils"
)

//GenDataKey generates a random key for data encryption
func GenDataKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// EncFile encrypts the file inName to outName with a random 32 byte key. returns the key
// assumes infile and outfile use they system seperator
func EncFile(infile, outfile string, key []byte) (d *digest.Digest, size int64, err error) {
	inFH, err := os.Open(infile)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "could not open file %s", infile)
	}
	defer func() {
		err = utils.CheckedClose(inFH, err)
	}()

	outFH, err := os.Create(outfile)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "could not create file %s", outfile)
	}
	defer func() {
		err = utils.CheckedClose(outFH, err)
	}()

	cfg := sio.Config{
		MinVersion:   sio.Version20,
		MaxVersion:   sio.Version20,
		CipherSuites: []byte{sio.AES_256_GCM},
		Key:          key,
	}

	digester := digest.Canonical.Digester()
	mw := io.MultiWriter(digester.Hash(), outFH)
	if size, err = sio.Encrypt(mw, inFH, cfg); err != nil {
		err2 := outFH.Close()
		err3 := os.Remove(outfile)
		// TODO: figure error reporting
		if err3 != nil {
			err3 = errors.Wrapf(err3, "warning, unauthenticated file could not be removed: %s", outfile)
		}
		return nil, 0, utils.Errors{err, err2, err3}
		//return nil, 0, utils.ErrEncrypt
	}

	ds := digester.Digest()
	return &ds, size, nil
}

// DecFile decrypts (and authenticates) infile and writes it to outfile
// only persists if the decrypttion and authentication suceedes
// assumes infile and outfile use they system seperator
func DecFile(infile, outfile string, datakey []byte) (err error) {
	inFH, err := os.Open(infile)
	if err != nil {
		return errors.Wrapf(err, "could not open file %s", infile)
	}
	defer func() {
		err = utils.CheckedClose(inFH, err)
	}()

	outFH, err := os.Create(outfile)
	if err != nil {
		return errors.Wrapf(err, "could not create file %s", outfile)
	}
	defer func() {
		err = utils.CheckedClose(outFH, err)
	}()

	cfg := sio.Config{
		MinVersion:   sio.Version20,
		MaxVersion:   sio.Version20,
		CipherSuites: []byte{sio.AES_256_GCM},
		Key:          datakey,
	}

	if _, err = sio.Decrypt(outFH, inFH, cfg); err != nil {
		err2 := outFH.Close()
		err3 := os.Remove(outfile)
		if err3 != nil {
			err3 = errors.Wrapf(err3, "warning, unauthenticated file could not be removed: %s", outfile)
		}
		return utils.Errors{err, err2, err3}
		//return utils.ErrDecrypt
	}

	return nil
}
