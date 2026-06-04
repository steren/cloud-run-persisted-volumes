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
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
)

func main() {
	log.Println("Initializing Cloud Run GCS Sidecar...")

	// 1. Load configuration from environment variables
	bucketName := os.Getenv("GCS_BUCKET")
	if bucketName == "" {
		log.Fatal("FATAL: GCS_BUCKET environment variable is required")
	}

	gcsPrefix := os.Getenv("GCS_PREFIX")
	if gcsPrefix == "" {
		gcsPrefix = "shared-data/"
	}

	sharedDir := os.Getenv("SHARED_DIR")
	if sharedDir == "" {
		sharedDir = "/data"
	}

	syncIntervalStr := os.Getenv("SYNC_INTERVAL")
	if syncIntervalStr == "" {
		syncIntervalStr = "1m"
	}
	syncInterval, err := time.ParseDuration(syncIntervalStr)
	if err != nil {
		log.Fatalf("FATAL: Invalid SYNC_INTERVAL duration '%s': %v", syncIntervalStr, err)
	}

	readyPort := os.Getenv("READY_PORT")
	if readyPort == "" {
		readyPort = "8080"
	}

	log.Printf("Configuration:")
	log.Printf("  GCS_BUCKET:     %s", bucketName)
	log.Printf("  GCS_PREFIX:     %s", gcsPrefix)
	log.Printf("  SHARED_DIR:     %s", sharedDir)
	log.Printf("  SYNC_INTERVAL:  %v", syncInterval)
	log.Printf("  READY_PORT:     %s", readyPort)

	// 2. Setup health HTTP server
	var startupDone atomic.Bool

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if startupDone.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("sync in progress"))
		}
	})

	server := &http.Server{
		Addr: ":" + readyPort,
	}

	go func() {
		log.Printf("Starting HTTP server on port %s...", readyPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("FATAL: HTTP server failed: %v", err)
		}
	}()

	// Create storage client
	ctx := context.Background()
	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("FATAL: Failed to create GCS client: %v", err)
	}
	defer storageClient.Close()

	// 3. Perform initial startup download from Cloud Storage
	log.Println("Executing initial startup download from Cloud Storage...")
	if err := DownloadDirectory(ctx, storageClient, bucketName, gcsPrefix, sharedDir); err != nil {
		log.Fatalf("FATAL: Initial startup download failed: %v", err)
	}

	// Signal readiness
	log.Println("Initial download completed successfully. Signaling readiness.")
	startupDone.Store(true)

	// 4. Setup ticker and signal handlers
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	log.Println("Entering main synchronization loop.")
	for {
		select {
		case <-ticker.C:
			log.Println("Periodic sync triggered...")
			syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			if err := UploadDirectory(syncCtx, storageClient, bucketName, gcsPrefix, sharedDir); err != nil {
				log.Printf("Error during periodic upload sync: %v", err)
			}
			cancel()

		case sig := <-sigs:
			log.Printf("Received termination signal (%v). Commencing graceful shutdown.", sig)
			ticker.Stop()

			// Shutdown HTTP server so no new ready probes or traffic is sent to us (though we are a sidecar)
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = server.Shutdown(shutdownCtx)
			shutdownCancel()

			// Perform final sync upload
			log.Println("Executing final upload sync before exit...")
			finalSyncCtx, finalCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if err := UploadDirectory(finalSyncCtx, storageClient, bucketName, gcsPrefix, sharedDir); err != nil {
				log.Printf("Error during final upload sync: %v", err)
				finalCancel()
				os.Exit(1)
			}
			finalCancel()

			log.Println("Graceful shutdown completed successfully. Exiting.")
			os.Exit(0)
		}
	}
}
