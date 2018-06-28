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

package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/docker/docker/client"
	"github.com/google/uuid"
	tarinator "github.com/verybluebot/tarinator-go"

	"github.com/Senetas/crypto-cli/registry"
	"github.com/Senetas/crypto-cli/utils"
)

const labelString = "LABEL com.senetas.crypto.enabled=true"

// PushImage encrypts then pushes an image
func PushImage(imageID string) error {
	layers, img, err := obtainImageData(imageID)
	if err != nil {
		return err
	}

	// output image
	imgName := uuid.New().String()
	path := os.TempDir() + "/com.senetas.crypto/"
	imgFile := path + imgName + ".tar"

	if err = os.MkdirAll(path+imgName, 0755); err != nil {
		return err
	}

	outFile, err := os.Create(imgFile)
	if err != nil {
		return err
	}
	defer utils.CheckedClose(outFile)

	if _, err = io.Copy(outFile, img); err != nil {
		return err
	}

	if err = outFile.Sync(); err != nil {
		return err
	}

	go func() {
		if err := img.Close(); err != nil {
			log.Println(err)
		}
	}()

	if err = tarinator.UnTarinate(path+imgName, imgFile); err != nil {
		return err
	}

	go func() {
		if err := os.Remove(imgFile); err != nil {
			log.Println(err)
		}
	}()

	layerSet := make(map[string]bool)
	for _, x := range layers {
		layerSet[x] = true
	}

	configData, layerData, err := findLayers(imageID, path+imgName, layerSet)
	if err != nil {
		return err
	}

	user := "narthanaepa1"
	repo := "narthanaepa1/my-alpine"
	tag := "crypto"

	service := "registry.docker.io"
	authServer := "auth.docker.io"

	authToken, err := registry.AuthToken()
	if err != nil {
		return err
	}

	token, err := registry.Authenticate(user, service, repo, authServer, authToken)

	manifest := assembleManifest(configData, layerData)

	mdigest, err := registry.PushManifest(user, repo, tag, token, manifest)
	if err != nil {
		return err
	}

	fmt.Print("Successfully uploaded manifest with digest: ")
	fmt.Println(mdigest)

	if err = registry.PushLayer(user, repo, tag, token, configData); err != nil {
		return err
	}

	for _, l := range layerData {
		if err = registry.PushLayer(user, repo, tag, token, l); err != nil {
			return err
		}
	}

	fmt.Println("Layers and config uploaded successfully")

	if err = os.RemoveAll(path + imgName); err != nil {
		return err
	}

	return nil
}

func obtainImageData(imageID string) ([]string, io.ReadCloser, error) {
	ctx := context.Background()

	// TODO: fix hardcoded version/ check if necessary
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.37"))
	if err != nil {
		return nil, nil, err
	}

	// get the history
	hist, err := cli.ImageHistory(ctx, imageID)
	if err != nil {
		return nil, nil, err
	}

	// obtain the most recent two complete images
	ids := []string{hist[0].ID}

	// advance pointer to history entry for LABEL "com.senetas.crypto.enabled=true"
	i := 0
	for ; i < len(hist) && !strings.Contains(hist[i].CreatedBy, labelString); i++ {
	}
	if i >= len(hist)-1 {
		return nil, nil, errors.New("no " + labelString + " in Dockerfile")
	}
	ids = append(ids, hist[i+1].ID)

	// map the layers of the two tags, since one tag was based on the other,
	// the layers of the lower tag should be duplicates of the upper one
	layerMap := make(map[string]int)
	for _, x := range ids {
		inspt, _, err := cli.ImageInspectWithRaw(ctx, x)
		if err != nil {
			return nil, nil, err
		}

		for _, x := range inspt.RootFS.Layers {
			layerMap[x]++
		}
	}

	layers := []string{}
	for k, v := range layerMap {
		if v == 1 {
			layers = append(layers, k)
		}
	}

	inspt, _, err := cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return nil, nil, err
	}

	img, err := cli.ImageSave(ctx, []string{inspt.ID})
	if err != nil {
		return nil, nil, err
	}

	return layers, img, nil
}
