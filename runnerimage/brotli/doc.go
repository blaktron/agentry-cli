// Package brotli decodes the Brotli compressed data format (RFC 7932). It is
// the reader of github.com/andybalholm/brotli v1.2.6, which is released under
// the MIT licence (the LICENSE file beside this one), with the compressor
// left out. The runner image's build context carries no module dependencies,
// so the decoder is part of it rather than a module the build would fetch.
package brotli
