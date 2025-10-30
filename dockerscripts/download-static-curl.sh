#!/bin/bash

function download_arch_specific_executable {
	curl -f -L -s -q \
		https://github.com/moparisthebest/static-curl/releases/latest/download/curl-amd64 \
		-o /go/bin/curl || exit 1
	chmod +x /go/bin/curl
}
