#!/usr/bin/env bash
set -euo pipefail

BINARY="looptui"
DEST="$HOME/dev/bin"

echo "Building $BINARY..."
go build -o "$BINARY" .

mkdir -p "$DEST"
mv "$BINARY" "$DEST/$BINARY"
echo "Installed to $DEST/$BINARY"
