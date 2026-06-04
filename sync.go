// Copyright 2026 Google LLC
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

package main

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// DownloadDirectory downloads all files from GCS bucket under gcsPrefix to localDir.
func DownloadDirectory(ctx context.Context, client *storage.Client, bucketName, gcsPrefix, localDir string) error {
	bucket := client.Bucket(bucketName)
	prefix := normalizePrefix(gcsPrefix)

	log.Printf("Starting initial download from gs://%s/%s to %s", bucketName, prefix, localDir)

	// Ensure local directory exists
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("failed to create local directory %s: %w", localDir, err)
	}

	query := &storage.Query{Prefix: prefix}
	it := bucket.Objects(ctx, query)

	downloadedCount := 0
	skippedCount := 0

	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to iterate GCS objects: %w", err)
		}

		// Skip "directories" (objects ending with trailing slash in GCS representation)
		if strings.HasSuffix(attrs.Name, "/") {
			continue
		}

		// Calculate relative path from GCS prefix
		relPath := strings.TrimPrefix(attrs.Name, prefix)
		relPath = strings.TrimPrefix(relPath, "/")
		if relPath == "" {
			continue
		}

		localPath := filepath.Join(localDir, filepath.FromSlash(relPath))

		// Ensure directories for the local file exist
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for local file %s: %w", localPath, err)
		}

		// Check if local file exists and matches size and MD5 hash to avoid redundant download
		if fileInfo, err := os.Stat(localPath); err == nil {
			if fileInfo.Size() == attrs.Size {
				localMD5, err := calculateMD5(localPath)
				if err == nil && matchesMD5(localMD5, attrs.MD5) {
					log.Printf("Skipping download of %s (already up to date)", attrs.Name)
					skippedCount++
					continue
				}
			}
		}

		// Download GCS object
		if err := downloadFile(ctx, bucket, attrs.Name, localPath); err != nil {
			return fmt.Errorf("failed to download %s: %w", attrs.Name, err)
		}

		log.Printf("Successfully downloaded %s -> %s (%d bytes)", attrs.Name, localPath, attrs.Size)
		downloadedCount++
	}

	log.Printf("Initial download complete. Downloaded: %d files, Skipped: %d files.", downloadedCount, skippedCount)
	return nil
}

// UploadDirectory uploads new or modified files from localDir to GCS bucket under gcsPrefix, and deletes removed files.
func UploadDirectory(ctx context.Context, client *storage.Client, bucketName, gcsPrefix, localDir string) error {
	bucket := client.Bucket(bucketName)
	prefix := normalizePrefix(gcsPrefix)

	log.Printf("Starting directory upload sync from %s to gs://%s/%s", localDir, bucketName, prefix)

	// Step 1: List all existing GCS objects under the prefix to build a metadata cache
	gcsFiles := make(map[string]*storage.ObjectAttrs)
	query := &storage.Query{Prefix: prefix}
	it := bucket.Objects(ctx, query)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to list existing GCS objects for diff: %w", err)
		}
		if !strings.HasSuffix(attrs.Name, "/") {
			gcsFiles[attrs.Name] = attrs
		}
	}

	uploadedCount := 0
	skippedCount := 0
	encounteredGCSKeys := make(map[string]bool)

	// Step 2: Walk the local directory recursively
	err := filepath.WalkDir(localDir, func(localPath string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		// Get relative path using forward slashes
		relPath, err := filepath.Rel(localDir, localPath)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", localPath, err)
		}
		gcsKey := path.Join(prefix, filepath.ToSlash(relPath))
		encounteredGCSKeys[gcsKey] = true

		fileInfo, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to get file info for %s: %w", localPath, err)
		}

		// Check if remote object exists and matches size & MD5 hash
		if gcsAttrs, exists := gcsFiles[gcsKey]; exists {
			if fileInfo.Size() == gcsAttrs.Size {
				localMD5, err := calculateMD5(localPath)
				if err == nil && matchesMD5(localMD5, gcsAttrs.MD5) {
					// Identical file, skip upload
					skippedCount++
					return nil
				}
			}
		}

		// Upload modified/new file
		if err := uploadFile(ctx, bucket, localPath, gcsKey); err != nil {
			return fmt.Errorf("failed to upload %s to %s: %w", localPath, gcsKey, err)
		}

		log.Printf("Successfully uploaded %s -> gs://%s/%s (%d bytes)", localPath, bucketName, gcsKey, fileInfo.Size())
		uploadedCount++
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed walking local directory: %w", err)
	}

	// Step 3: Handle deletion of removed files
	deletedCount := 0
	for gcsKey := range gcsFiles {
		if !encounteredGCSKeys[gcsKey] {
			log.Printf("Deleting removed file from GCS: gs://%s/%s", bucketName, gcsKey)
			if err := bucket.Object(gcsKey).Delete(ctx); err != nil {
				log.Printf("Warning: failed to delete gs://%s/%s: %v", bucketName, gcsKey, err)
			} else {
				deletedCount++
			}
		}
	}

	log.Printf("Upload sync complete. Uploaded: %d, Skipped: %d, Deleted from GCS: %d.", uploadedCount, skippedCount, deletedCount)
	return nil
}

// Helpers

func normalizePrefix(p string) string {
	p = strings.TrimPrefix(p, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p = p + "/"
	}
	return p
}

func calculateMD5(filePath string) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func matchesMD5(local []byte, remote []byte) bool {
	if len(local) != len(remote) {
		return false
	}
	for i := range local {
		if local[i] != remote[i] {
			return false
		}
	}
	return true
}

func downloadFile(ctx context.Context, bucket *storage.BucketHandle, gcsKey, localPath string) error {
	reader, err := bucket.Object(gcsKey).NewReader(ctx)
	if err != nil {
		return err
	}
	defer reader.Close()

	writer, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer writer.Close()

	_, err = io.Copy(writer, reader)
	return err
}

func uploadFile(ctx context.Context, bucket *storage.BucketHandle, localPath, gcsKey string) error {
	reader, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	writer := bucket.Object(gcsKey).NewWriter(ctx)
	defer writer.Close()

	_, err = io.Copy(writer, reader)
	if err != nil {
		return err
	}

	return writer.Close()
}
