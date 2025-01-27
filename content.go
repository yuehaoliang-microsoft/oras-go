/*
Copyright The ORAS Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package oras

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/internal/cas"
	"oras.land/oras-go/v2/internal/docker"
	"oras.land/oras-go/v2/internal/platform"
	"oras.land/oras-go/v2/registry"
)

// Tag tags the descriptor identified by src with dst.
func Tag(ctx context.Context, target Target, src, dst string) error {
	if refTagger, ok := target.(registry.ReferenceTagger); ok {
		return refTagger.TagReference(ctx, src, dst)
	}
	refFetcher, okFetch := target.(registry.ReferenceFetcher)
	refPusher, okPush := target.(registry.ReferencePusher)
	if okFetch && okPush {
		desc, rc, err := refFetcher.FetchReference(ctx, src)
		if err != nil {
			return err
		}
		defer rc.Close()
		return refPusher.PushReference(ctx, desc, rc, dst)
	}
	desc, err := target.Resolve(ctx, src)
	if err != nil {
		return err
	}
	return target.Tag(ctx, desc, dst)
}

// DefaultResolveOptions provides the default ResolveOptions.
var DefaultResolveOptions ResolveOptions

// defaultResolveMaxMetadataBytes is the default value of
// ResolveOptions.MaxMetadataBytes.
const defaultResolveMaxMetadataBytes int64 = 4 * 1024 * 1024 // 4 MiB

// ResolveOptions contains parameters for oras.Resolve.
type ResolveOptions struct {
	// TargetPlatform ensures the resolved content matches the target platform
	// if the node is a manifest, or selects the first resolved content that
	// matches the target platform if the node is a manifest list.
	TargetPlatform *ocispec.Platform

	// MaxMetadataBytes limits the maximum size of metadata that can be cached
	// in the memory.
	// If less than or equal to 0, a default (currently 4 MiB) is used.
	MaxMetadataBytes int64
}

// Resolve resolves a descriptor with provided reference from the target.
func Resolve(ctx context.Context, target ReadOnlyTarget, reference string, opts ResolveOptions) (ocispec.Descriptor, error) {
	if opts.TargetPlatform == nil {
		return target.Resolve(ctx, reference)
	}
	return resolve(ctx, target, nil, reference, opts)
}

// resolve resolves a descriptor with provided reference from the target, with
// specified caching.
func resolve(ctx context.Context, target ReadOnlyTarget, proxy *cas.Proxy, reference string, opts ResolveOptions) (ocispec.Descriptor, error) {
	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = defaultResolveMaxMetadataBytes
	}

	if refFetcher, ok := target.(registry.ReferenceFetcher); ok {
		// optimize performance for ReferenceFetcher targets
		desc, rc, err := refFetcher.FetchReference(ctx, reference)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		defer rc.Close()

		switch desc.MediaType {
		case docker.MediaTypeManifestList, ocispec.MediaTypeImageIndex,
			docker.MediaTypeManifest, ocispec.MediaTypeImageManifest:
			// cache the fetched content
			if desc.Size > opts.MaxMetadataBytes {
				return ocispec.Descriptor{}, fmt.Errorf(
					"content size %v exceeds MaxMetadataBytes %v: %w",
					desc.Size,
					opts.MaxMetadataBytes,
					errdef.ErrSizeExceedsLimit)
			}
			if proxy == nil {
				proxy = cas.NewProxyWithLimit(target, cas.NewMemory(), opts.MaxMetadataBytes)
			}
			if err := proxy.Cache.Push(ctx, desc, rc); err != nil {
				return ocispec.Descriptor{}, err
			}
			// stop caching as SelectManifest may fetch a config blob
			proxy.StopCaching = true
			return platform.SelectManifest(ctx, proxy, desc, opts.TargetPlatform)
		default:
			return ocispec.Descriptor{}, fmt.Errorf("%s: %s: %w", desc.Digest, desc.MediaType, errdef.ErrUnsupported)
		}
	}

	desc, err := target.Resolve(ctx, reference)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return platform.SelectManifest(ctx, target, desc, opts.TargetPlatform)
}

// DefaultFetchOptions provides the default FetchOptions.
var DefaultFetchOptions FetchOptions

// FetchOptions contains parameters for oras.Fetch.
type FetchOptions struct {
	// ResolveOptions contains parameters for resolving reference.
	ResolveOptions
}

// Fetch fetches the content identified by the reference.
func Fetch(ctx context.Context, target ReadOnlyTarget, reference string, opts FetchOptions) (ocispec.Descriptor, io.ReadCloser, error) {
	if opts.TargetPlatform == nil {
		if refFetcher, ok := target.(registry.ReferenceFetcher); ok {
			return refFetcher.FetchReference(ctx, reference)
		}

		desc, err := target.Resolve(ctx, reference)
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		rc, err := target.Fetch(ctx, desc)
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}
		return desc, rc, nil
	}

	if opts.MaxMetadataBytes <= 0 {
		opts.MaxMetadataBytes = defaultResolveMaxMetadataBytes
	}
	proxy := cas.NewProxyWithLimit(target, cas.NewMemory(), opts.MaxMetadataBytes)
	desc, err := resolve(ctx, target, proxy, reference, opts.ResolveOptions)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	// if the content exists in cache, fetch it from cache
	// otherwise fetch without caching
	proxy.StopCaching = true
	rc, err := proxy.Fetch(ctx, desc)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	return desc, rc, nil
}

// DefaultFetchBytesOptions provides the default FetchBytesOptions.
var DefaultFetchBytesOptions FetchBytesOptions

// defaultMaxBytes is the default value of FetchBytesOptions.MaxBytes.
const defaultMaxBytes int64 = 4 * 1024 * 1024 // 4 MiB

// FetchBytesOptions contains parameters for oras.FetchBytes.
type FetchBytesOptions struct {
	// FetchOptions contains parameters for fetching content.
	FetchOptions
	// MaxBytes limits the maximum size of the fetched content bytes.
	// If less than or equal to 0, a default (currently 4 MiB) is used.
	MaxBytes int64
}

// FetchBytes fetches the content bytes identified by the reference.
func FetchBytes(ctx context.Context, target ReadOnlyTarget, reference string, opts FetchBytesOptions) (ocispec.Descriptor, []byte, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}

	desc, rc, err := Fetch(ctx, target, reference, opts.FetchOptions)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	defer rc.Close()

	if desc.Size > opts.MaxBytes {
		return ocispec.Descriptor{}, nil, fmt.Errorf(
			"content size %v exceeds MaxBytes %v: %w",
			desc.Size,
			opts.MaxBytes,
			errdef.ErrSizeExceedsLimit)
	}
	bytes, err := content.ReadAll(rc, desc)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}

	return desc, bytes, nil
}

// PushBytes describes the contentBytes using the given mediaType and pushes it.
// If mediaType is not specified, "application/octet-stream" is used.
func PushBytes(ctx context.Context, pusher content.Pusher, mediaType string, contentBytes []byte) (ocispec.Descriptor, error) {
	desc := content.NewDescriptorFromBytes(mediaType, contentBytes)
	r := bytes.NewReader(contentBytes)
	if err := pusher.Push(ctx, desc, r); err != nil {
		return ocispec.Descriptor{}, err
	}

	return desc, nil
}

// defaultTagConcurrency is the default value of TagBytesNOptions.Concurrency.
const defaultTagConcurrency = 5 // This value is consistent with dockerd

// DefaultTagBytesNOptions provides the default TagBytesNOptions.
var DefaultTagBytesNOptions TagBytesNOptions

// TagBytesNOptions contains parameters for oras.TagBytesN.
type TagBytesNOptions struct {
	// Concurrency limits the maximum number of concurrent tag tasks.
	// If less than or equal to 0, a default (currently 5) is used.
	Concurrency int64
}

// TagBytesN describes the contentBytes using the given mediaType, pushes it,
// and tag it with the given references.
// If mediaType is not specified, "application/octet-stream" is used.
func TagBytesN(ctx context.Context, target Target, mediaType string, contentBytes []byte, references []string, opts TagBytesNOptions) (ocispec.Descriptor, error) {
	if len(references) == 0 {
		return PushBytes(ctx, target, mediaType, contentBytes)
	}

	desc := content.NewDescriptorFromBytes(mediaType, contentBytes)
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultTagConcurrency
	}
	limiter := semaphore.NewWeighted(opts.Concurrency)
	eg, egCtx := errgroup.WithContext(ctx)
	if refPusher, ok := target.(registry.ReferencePusher); ok {
		for _, reference := range references {
			limiter.Acquire(ctx, 1)
			eg.Go(func(ref string) func() error {
				return func() error {
					defer limiter.Release(1)
					r := bytes.NewReader(contentBytes)
					if err := refPusher.PushReference(egCtx, desc, r, ref); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
						return err
					}
					return nil
				}
			}(reference))
		}
	} else {
		r := bytes.NewReader(contentBytes)
		if err := target.Push(ctx, desc, r); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
			return ocispec.Descriptor{}, err
		}

		for _, reference := range references {
			limiter.Acquire(ctx, 1)
			eg.Go(func(ref string) func() error {
				return func() error {
					defer limiter.Release(1)
					return target.Tag(egCtx, desc, ref)
				}
			}(reference))
		}
	}

	if err := eg.Wait(); err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}

// TagBytes describes the contentBytes using the given mediaType, pushes it,
// and tag it with the given reference.
// If mediaType is not specified, "application/octet-stream" is used.
func TagBytes(ctx context.Context, target Target, mediaType string, contentBytes []byte, reference string) (ocispec.Descriptor, error) {
	return TagBytesN(ctx, target, mediaType, contentBytes, []string{reference}, DefaultTagBytesNOptions)
}
