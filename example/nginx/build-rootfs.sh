#!/bin/sh
# Legacy helper for the low-level `make nginx` target. The Macker workflow
# performs this assembly directly from example/nginx/Mackerfile using RUN.
set -eu

output=${1:?usage: build-rootfs.sh OUTPUT CONFIG}
config=${2:?usage: build-rootfs.sh OUTPUT CONFIG}

if [ "$(uname -s)" != "Darwin" ]; then
    echo "nginx Darwin rootfs must be built on macOS" >&2
    exit 1
fi

command -v brew >/dev/null 2>&1 || {
    echo "brew is required to locate nginx and its libraries" >&2
    exit 1
}
command -v install_name_tool >/dev/null 2>&1 || {
    echo "install_name_tool is required to make the nginx binary relocatable" >&2
    exit 1
}
command -v codesign >/dev/null 2>&1 || {
    echo "codesign is required after rewriting Mach-O load paths" >&2
    exit 1
}

homebrew_prefix=$(brew --prefix)
nginx_prefix=$(brew --prefix nginx)
pcre2_prefix=$(brew --prefix pcre2)
openssl_prefix=$(brew --prefix openssl@3)
ca_certificates_prefix=$(brew --prefix ca-certificates)
mime_source="$homebrew_prefix/etc/nginx/mime.types"

nginx_source="$nginx_prefix/bin/nginx"
pcre2_source="$pcre2_prefix/lib/libpcre2-8.0.dylib"
ssl_source="$openssl_prefix/lib/libssl.3.dylib"
crypto_source="$openssl_prefix/lib/libcrypto.3.dylib"

for file in "$nginx_source" "$pcre2_source" "$ssl_source" "$crypto_source" \
    "$mime_source" "$ca_certificates_prefix/share/ca-certificates/cacert.pem"; do
    if [ ! -e "$file" ]; then
        echo "required Homebrew file is missing: $file" >&2
        exit 1
    fi
done

rm -rf "$output"
mkdir -p \
    "$output/usr/local/bin" \
    "$output/usr/local/lib" \
    "$output/etc/nginx" \
    "$output/etc/ssl" \
    "$output/var/log/nginx" \
    "$output/var/run" \
    "$output/www"

nginx_binary="$output/usr/local/bin/nginx"
pcre2_library="$output/usr/local/lib/libpcre2-8.0.dylib"
ssl_library="$output/usr/local/lib/libssl.3.dylib"
crypto_library="$output/usr/local/lib/libcrypto.3.dylib"

cp -L "$nginx_source" "$nginx_binary"
cp -L "$pcre2_source" "$pcre2_library"
cp -L "$ssl_source" "$ssl_library"
cp -L "$crypto_source" "$crypto_library"
cp -L "$config" "$output/etc/nginx/nginx.conf"
cp -L "$mime_source" "$output/etc/nginx/mime.types"
cp -L "$ca_certificates_prefix/share/ca-certificates/cacert.pem" "$output/etc/ssl/cert.pem"

# Homebrew's nginx refers to libraries through absolute /opt/homebrew paths.
# Rewrite those references so the image carries its Homebrew dependencies next
# to the binary instead of requiring the same Homebrew installation on the
# destination Mac. macOS system libraries remain host-provided.
install_name_tool -change "$pcre2_source" \
    '@loader_path/../lib/libpcre2-8.0.dylib' "$nginx_binary"
install_name_tool -change "$ssl_source" \
    '@loader_path/../lib/libssl.3.dylib' "$nginx_binary"
install_name_tool -change "$crypto_source" \
    '@loader_path/../lib/libcrypto.3.dylib' "$nginx_binary"

ssl_crypto_dependency=$(otool -L "$ssl_source" | awk '$1 ~ /libcrypto/ { print $1; exit }')
if [ -z "$ssl_crypto_dependency" ]; then
    echo "could not find libssl's libcrypto dependency" >&2
    exit 1
fi
install_name_tool -change "$ssl_crypto_dependency" \
    '@loader_path/libcrypto.3.dylib' "$ssl_library"

# install_name_tool invalidates the Homebrew ad-hoc signatures. Re-sign the
# copied files ad hoc; this is not a release identity or a security boundary.
codesign --force --sign - "$nginx_binary" "$pcre2_library" "$ssl_library" "$crypto_library" >/dev/null

chmod 0755 "$nginx_binary"
echo "created Darwin nginx rootfs at $output"
otool -L "$nginx_binary"
