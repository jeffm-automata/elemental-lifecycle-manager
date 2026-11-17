/*
Copyright © 2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package recipe carries the image build recipe inside the operator binary. The files under
// files/ are a verbatim copy (see files/SOURCE) and are not edited here; "make sync-recipe"
// refreshes them.
package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed all:files
var files embed.FS

const root = "files"

// EntryPoint is the script, relative to the unpacked recipe, that builds an image.
const EntryPoint = "build.sh"

func list() ([]string, error) {
	var paths []string
	err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

// Digest identifies the recipe: sha256 over every file's path, length and content, in path order.
func Digest() (string, error) {
	paths, err := list()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, p := range paths {
		data, err := files.ReadFile(p)
		if err != nil {
			return "", err
		}
		h.Write(fmt.Appendf(nil, "%s\x00%d\x00", strings.TrimPrefix(p, root+"/"), len(data)))
		h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// Tarball returns the recipe as a gzip-compressed tar archive. Scripts are executable.
func Tarball() ([]byte, error) {
	paths, err := list()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		data, err := files.ReadFile(p)
		if err != nil {
			return nil, err
		}
		mode := int64(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		hdr := &tar.Header{Name: strings.TrimPrefix(p, root+"/"), Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
