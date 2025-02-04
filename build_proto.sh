#! /bin/bash

_ROOT="$(pwd)" && cd "$(dirname "$0")" && ROOT="$(pwd)"
PJROOT="$ROOT"

if ! which protoc &> /dev/null
then
    echo "protoc uninstalled. Please install it first."
    exit 1
fi

if ! which protoc-gen-go &> /dev/null
then
    echo "protoc-gen-go uninstalled. Please install it first."
    exit 1
fi

SRC_DIR=$PJROOT/types
DST_DIR=$PJROOT
protoc -I=$PJROOT --go_out=$DST_DIR --go_opt=paths=source_relative $SRC_DIR/types.proto