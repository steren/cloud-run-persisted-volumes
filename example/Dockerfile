# Build and Run Stage
FROM node:20-alpine

# Set clean production environment
ENV NODE_ENV=production

WORKDIR /app

# Copy dependency manifests and install dependencies
COPY package.json ./
RUN npm install --only=production

# Copy source code and UI files
COPY index.js ./
COPY public ./public

# Ensure the shared data mount directory exists and is writable
RUN mkdir -p /data

# Expose default port
EXPOSE 3000

# Execute server
CMD ["node", "index.js"]
