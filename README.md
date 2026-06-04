# Google Cloud Run GCS Directory Sync Sidecar

A lightweight, robust, and highly optimized Google Cloud Run sidecar container written in Go that synchronizes an in-memory shared volume (`emptyDir`) with Google Cloud Storage (GCS).

This sidecar implements a **bidirectional-on-lifecycle** synchronization pattern designed for stateless containers that need access to persistent, shared file systems (e.g., file assets, static sites, or simple file databases) with high-speed in-memory reads/writes:
1. **On Startup**: It downloads all files from a specified GCS bucket/prefix into the shared directory. It blocks the main application's startup until the initial download is 100% complete.
2. **Periodically (Configurable)**: It scans the local shared directory and uploads new or modified files to GCS. It uses file size and MD5 hash comparisons to perform high-performance **delta uploads**, avoiding redundant uploads and minimizing GCS egress costs and API write fees.
3. **On Shutdown (SIGTERM)**: It traps termination signals sent by Cloud Run, pauses active ticker runs, and executes a final, comprehensive upload sync before gracefully exiting.

---

## Architecture Overview

```
                      +---------------------------------------+
                      |          Cloud Run Instance           |
                      |                                       |
  [ HTTP Traffic ] ------> [ main-app ]                       |
                      |        |                              |
                      |        | (Reads/Writes)               |
                      |        v                              |
                      |   +===============================+   |
                      |   |  Shared In-Memory Volume      |   |
                      |   |  (/data, backed by tmpfs)     |   |
                      |   +===============================+   |
                      |        ^                              |
                      |        | (Syncs Dir)                  |
                      |        v                              |
                      |   [ gcs-sidecar ]                     |
                      |        |                              |
                      +--------|------------------------------+
                               |
                               | (HTTPS API)
                               v
                     +--------------------+
                     |    Google Cloud    |
                     |      Storage       |
                     +--------------------+
```

---

## Configuration Reference

The sidecar is configured entirely via environment variables. Configure these under the `gcs-sidecar` container spec inside your `service.yaml`.

| Environment Variable | Description | Default | Required? |
|----------------------|-------------|---------|-----------|
| `GCS_BUCKET` | The GCS bucket name to sync files with. | — | **Yes** |
| `GCS_PREFIX` | Prefix (folder) in GCS under which files are stored. | `shared-data/` | No |
| `SHARED_DIR` | The local mount path of the shared volume. | `/data` | No |
| `SYNC_INTERVAL` | Frequency of periodic upload checks. Parseable as Go duration (e.g., `10s`, `1m`, `5m`). | `1m` | No |
| `READY_PORT` | Port where readiness and liveness HTTP probes are exposed. | `8080` | No |

---

## Container Startup Ordering

Cloud Run multi-container services support specifying startup dependencies using the `run.googleapis.com/container-dependencies` annotation. 

The sidecar exposes two endpoints on its `READY_PORT`:
* `/healthz`: Always returns `200 OK` (liveness probe).
* `/ready`: Returns `503 Service Unavailable` on startup. Once the initial download from GCS completes successfully, it transitions to returning `200 OK` (readiness probe).

By defining a `startupProbe` pointing to `/ready` on the sidecar and specifying that `main-app` depends on `gcs-sidecar` (see `service.yaml`), Cloud Run guarantees that **the sidecar will download all files from GCS before the main application starts running**.

---

## Quick Start & Deployment Guide

### 1. Set Environment Variables

Define your deployment parameters in your terminal. These variables will be used for both building the container image and generating the deployment manifest:

```bash
export PROJECT_ID="your-project-id"
export REGION="us-central1"
export REPOSITORY="your-artifact-repo"
export GCS_BUCKET="your-gcs-bucket-name"
```

### 2. Build and Push the Sidecar Image

Compile, package, and push the sidecar image using Google Cloud Build (which uses our multi-stage `Dockerfile` and runs as the non-root `sidecar-user`):

```bash
gcloud builds submit --tag ${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPOSITORY}/gcs-sidecar:latest .
```

### 3. Build and Push the Main Application Image

Package and push the example Node.js main application container image using Google Cloud Build:

```bash
gcloud builds submit --tag ${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPOSITORY}/main-app:latest example/
```

### 4. Deploy to Cloud Run

1. Render the final `service.yaml` deployment manifest from the template (using `sed` which is built-in on macOS and Linux):
   ```bash
   sed -e "s/\${PROJECT_ID}/${PROJECT_ID}/g" \
       -e "s/\${REGION}/${REGION}/g" \
       -e "s/\${REPOSITORY}/${REPOSITORY}/g" \
       -e "s/\${GCS_BUCKET}/${GCS_BUCKET}/g" \
       service.template.yaml > service.yaml
   ```

2. Apply and deploy the generated configuration using `gcloud`:
   ```bash
    gcloud run services replace service.yaml
    ```

---

## Example Main App (Node.js)

An example main application is provided in the [example/](file:///Users/steren/Documents/persisted-volume/example/) directory. This application is built using Node.js and Express and provides a gorgeous, glassmorphic web dashboard allowing you to inspect, create, edit, and delete files directly inside the `/data` shared volume.

Saving files in the UI instantly triggers the sidecar periodic sync to GCS, and deleting files automatically deletes them from GCS too!

### 1. Run Locally
To test the example app locally:
```bash
cd example
npm install
SHARED_DIR=./data PORT=3000 npm start
```
Open [http://localhost:3000](http://localhost:3000) to view the file dashboard.

### 2. Build and Deploy
Build and push the main application image to Artifact Registry:
```bash
gcloud builds submit --tag ${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPOSITORY}/main-app:latest example/
```

---

## IAM Permissions & Security

For the sidecar to operate, the service account assigned to the Cloud Run service must have permissions to read, write, and delete objects in your target GCS bucket.

1. **Recommended Role**: Assign the **Storage Object Admin** (`roles/storage.objectAdmin`) or **Storage Object User** (`roles/storage.objectUser`) role to the Cloud Run service account on the specific bucket. 
2. **Deletion Permission**: Since the sidecar defaults to deleting GCS objects that have been removed locally, ensure the service account has the `storage.objects.delete` permission (which is included in the roles above).

---

## Key Design Optimizations

- **Highly Efficient Diffing**: Instead of calling GCS metadata endpoints for *every* local file recursively (which causes excessive HTTP overhead and is slow), the sidecar listings are loaded into a memory cache map once at the beginning of each sync cycle. Local file sizes and MD5 hashes are compared against the cache map to only write modified files.
- **Atomic File Writing**: File writes to the local file system on download are done cleanly. Directories are resolved dynamically.
- **Safe Signal Handling**: Once SIGTERM is received, the sidecar suspends its periodic execution ticker, shuts down incoming probe servers, and executes a final upload sync. Cloud Run holds the container alive during this period up to the configured container timeout, ensuring complete data replication on instance termination.
