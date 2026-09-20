package objectstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// This is a pure unit test (no Docker, no network): minio-go's presigning
// methods compute a signed URL locally and never contact the server as long
// as Region is set (PresignedPostPolicy would otherwise call GetBucketLocation
// to discover it), which Config.Region always is here.
//
// It proves the property ADR-005's 2026-09-08 update depends on: the browser
// dereferences a signed URL, not the app, so a presigning client configured
// with the public host must produce URLs on that host — while server-side
// calls keep using the private endpoint untouched.
func TestPresignUsesPublicEndpointWhenSet(t *testing.T) {
	store, err := objectstore.New(objectstore.Config{
		Endpoint: "minio:9000", Region: "us-east-1", Bucket: "media",
		AccessKey: "minioadmin", SecretKey: "minioadmin", UseSSL: false,
		PublicEndpoint: "localhost:9000", PublicUseSSL: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	up, err := store.PresignPost(ctx, "tenant-a/asset-1", time.Minute, objectstore.UploadConstraints{
		MaxBytes: 1024, ContentType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(up.URL, "http://localhost:9000/") {
		t.Fatalf("PresignPost URL=%q want http://localhost:9000/ prefix", up.URL)
	}
	if strings.Contains(up.URL, "minio:9000") {
		t.Fatalf("PresignPost URL=%q leaked the private endpoint", up.URL)
	}

	getURL, err := store.PresignGet(ctx, "tenant-a/asset-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(getURL, "http://localhost:9000/") {
		t.Fatalf("PresignGet URL=%q want http://localhost:9000/ prefix", getURL)
	}
}

// When PublicEndpoint is unset, behavior must be identical to before this
// change: presigned URLs use the private endpoint, and no second client is
// built.
func TestPresignFallsBackToPrivateEndpointWhenUnset(t *testing.T) {
	store, err := objectstore.New(objectstore.Config{
		Endpoint: "minio:9000", Region: "us-east-1", Bucket: "media",
		AccessKey: "minioadmin", SecretKey: "minioadmin", UseSSL: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	up, err := store.PresignPost(ctx, "tenant-a/asset-1", time.Minute, objectstore.UploadConstraints{
		MaxBytes: 1024, ContentType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(up.URL, "http://minio:9000/") {
		t.Fatalf("PresignPost URL=%q want http://minio:9000/ prefix", up.URL)
	}

	getURL, err := store.PresignGet(ctx, "tenant-a/asset-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(getURL, "http://minio:9000/") {
		t.Fatalf("PresignGet URL=%q want http://minio:9000/ prefix", getURL)
	}
}
