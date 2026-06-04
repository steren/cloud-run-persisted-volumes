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

const express = require('express');
const fs = require('fs').promises;
const path = require('path');

const app = express();
const PORT = process.env.PORT || 3000;
const SHARED_DIR = process.env.SHARED_DIR || '/data';

app.use(express.json());
app.use(express.static(path.join(__dirname, 'public')));

// Helper to sanitize filename and prevent path traversal
function getSafePath(filename) {
  if (!filename) return null;
  // Allow only standard alphanumeric, dots, dashes, and underscores
  const cleanName = filename.replace(/[^a-zA-Z0-9.\-_]/g, '');
  if (cleanName === '' || cleanName.includes('..')) {
    return null;
  }
  return path.join(SHARED_DIR, cleanName);
}

// Ensure the shared directory exists
async function ensureDir() {
  try {
    await fs.mkdir(SHARED_DIR, { recursive: true });
    console.log(`Shared directory is active at: ${SHARED_DIR}`);
  } catch (err) {
    console.error(`Warning: Could not create shared directory ${SHARED_DIR}:`, err.message);
  }
}

// 1. List all files in the shared directory
app.get('/api/files', async (req, res) => {
  try {
    await fs.mkdir(SHARED_DIR, { recursive: true });
    const files = await fs.readdir(SHARED_DIR);
    const fileDetails = [];

    for (const file of files) {
      const fullPath = path.join(SHARED_DIR, file);
      try {
        const stat = await fs.stat(fullPath);
        if (stat.isFile()) {
          fileDetails.push({
            name: file,
            size: stat.size,
            updatedAt: stat.mtime
          });
        }
      } catch (e) {
        // Skip files that fail to stat (e.g. permission issues or deleted during list)
      }
    }

    // Sort by updated time descending
    fileDetails.sort((a, b) => b.updatedAt - a.updatedAt);
    res.json({ success: true, files: fileDetails });
  } catch (err) {
    res.status(500).json({ success: false, error: err.message });
  }
});

// 2. Read file content
app.get('/api/files/:filename', async (req, res) => {
  const safePath = getSafePath(req.params.filename);
  if (!safePath) {
    return res.status(400).json({ success: false, error: 'Invalid or unsafe filename' });
  }

  try {
    const content = await fs.readFile(safePath, 'utf8');
    res.json({ success: true, name: req.params.filename, content });
  } catch (err) {
    if (err.code === 'ENOENT') {
      res.status(404).json({ success: false, error: 'File not found' });
    } else {
      res.status(500).json({ success: false, error: err.message });
    }
  }
});

// 3. Create or update file content
app.post('/api/files/:filename', async (req, res) => {
  const safePath = getSafePath(req.params.filename);
  if (!safePath) {
    return res.status(400).json({ success: false, error: 'Invalid or unsafe filename' });
  }

  const { content } = req.body;
  if (content === undefined) {
    return res.status(400).json({ success: false, error: 'Missing content field in request body' });
  }

  try {
    await fs.mkdir(path.dirname(safePath), { recursive: true });
    await fs.writeFile(safePath, content, 'utf8');
    console.log(`Saved file: ${safePath} (${content.length} bytes)`);
    res.json({ success: true, message: 'File saved successfully' });
  } catch (err) {
    res.status(500).json({ success: false, error: err.message });
  }
});

// 4. Delete file
app.delete('/api/files/:filename', async (req, res) => {
  const safePath = getSafePath(req.params.filename);
  if (!safePath) {
    return res.status(400).json({ success: false, error: 'Invalid or unsafe filename' });
  }

  try {
    await fs.unlink(safePath);
    console.log(`Deleted file: ${safePath}`);
    res.json({ success: true, message: 'File deleted successfully' });
  } catch (err) {
    if (err.code === 'ENOENT') {
      res.status(404).json({ success: false, error: 'File does not exist' });
    } else {
      res.status(500).json({ success: false, error: err.message });
    }
  }
});

// Server bootup
ensureDir().then(() => {
  app.listen(PORT, () => {
    console.log(`==================================================`);
    console.log(`Node.js Example Main App running on port ${PORT}`);
    console.log(`Access the UI at: http://localhost:${PORT}`);
    console.log(`==================================================`);
  });
});
