#!/usr/bin/env bash
# Generate test files of various sizes and types for E2E upload tests.
# Usage: bash data/generate-test-files.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FILES_DIR="${SCRIPT_DIR}/files"

mkdir -p "$FILES_DIR"

echo "Generating test files in ${FILES_DIR}..."

# Text files
dd if=/dev/urandom bs=1024 count=1 2>/dev/null | base64 > "${FILES_DIR}/test-1kb.txt"
dd if=/dev/urandom bs=1024 count=100 2>/dev/null | base64 > "${FILES_DIR}/test-100kb.txt"
dd if=/dev/urandom bs=1024 count=8192 2>/dev/null | base64 > "${FILES_DIR}/test-8mb.txt"

# JSON file
echo '{"test": true, "data": "sample payload", "items": [1,2,3]}' > "${FILES_DIR}/test-2kb.json"
python3 -c "
import json, string, random
data = {'entries': [{'id': i, 'value': ''.join(random.choices(string.ascii_letters, k=50))} for i in range(20)]}
print(json.dumps(data, indent=2))
" > "${FILES_DIR}/test-2kb.json" 2>/dev/null || true

# CSV file
{
  echo "id,name,email,score"
  for i in $(seq 1 2000); do
    echo "${i},user_${i},user${i}@test.com,$((RANDOM % 100))"
  done
} > "${FILES_DIR}/test-200kb.csv"

# XML file
{
  echo '<?xml version="1.0" encoding="UTF-8"?>'
  echo '<root>'
  for i in $(seq 1 50); do
    echo "  <item id=\"${i}\"><name>Item ${i}</name><value>$((RANDOM % 1000))</value></item>"
  done
  echo '</root>'
} > "${FILES_DIR}/test-4kb.xml"

# HTML file
{
  echo '<!DOCTYPE html><html><head><title>Test</title></head><body>'
  echo '<h1>Test File</h1>'
  for i in $(seq 1 100); do
    echo "<p>Paragraph ${i}: Lorem ipsum dolor sit amet.</p>"
  done
  echo '</body></html>'
} > "${FILES_DIR}/test-8kb.html"

# SVG file
{
  echo '<svg xmlns="http://www.w3.org/2000/svg" width="200" height="200">'
  echo '  <rect width="200" height="200" fill="#4a90d9"/>'
  echo '  <circle cx="100" cy="100" r="80" fill="#fff" opacity="0.5"/>'
  echo '  <text x="100" y="110" text-anchor="middle" fill="#333" font-size="20">QURL</text>'
  echo '</svg>'
} > "${FILES_DIR}/test-3kb.svg"

# PNG file (1x1 pixel, minimal)
printf '\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde\x00\x00\x00\x0cIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82' > "${FILES_DIR}/test-10kb.png"
# Pad to 10KB
dd if=/dev/urandom bs=1 count=9900 >> "${FILES_DIR}/test-10kb.png" 2>/dev/null

# Binary file
dd if=/dev/urandom bs=1024 count=256 of="${FILES_DIR}/test-256kb.bin" 2>/dev/null

# ZIP file (zip the text file)
cd "${FILES_DIR}" && zip -q test-1mb.zip test-100kb.txt 2>/dev/null || true
cd "${SCRIPT_DIR}"

echo "Done! Generated files:"
ls -lh "${FILES_DIR}/"
