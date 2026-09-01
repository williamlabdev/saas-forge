package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

func dockerUp() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

// This suite proves the presign round-trip against a real S3-compatible server.
// A fake store can show that we CALL the right methods; only a real server shows
// that the signatures it produces are actually accepted — and, critically, that
// the bucket stays private to anyone without one.
func TestPresignRoundTrip(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:RELEASE.2024-09-13T20-26-02Z",
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": "minioadmin", "MINIO_ROOT_PASSWORD": "minioadmin"},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("minio container: %v", err)
	}
	defer func() { _ = container.Terminate(ctx) }()

	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "9000")
	endpoint := host + ":" + port.Port()

	admin, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4("minioadmin", "minioadmin", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.MakeBucket(ctx, "media", minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	store, err := objectstore.New(objectstore.Config{
		Endpoint: endpoint, Bucket: "media",
		AccessKey: "minioadmin", SecretKey: "minioadmin",
	})
	if err != nil {
		t.Fatal(err)
	}

	const key = "tenant-a/asset-1"
	body := []byte("hello media")

	// The client uploads straight to storage with a signed form — bytes never
	// pass through the platform.
	up, err := store.PresignPost(ctx, key, time.Minute, objectstore.UploadConstraints{
		MaxBytes: 1024, ContentType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := postUpload(ctx, up, body, "image/png"); err != nil {
		t.Fatal(err)
	} else if code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("presigned POST rejected: %d", code)
	}

	// Stat reports what actually landed — the platform records this rather than
	// the client's claim.
	info, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) {
		t.Fatalf("size=%d want %d", info.Size, len(body))
	}

	// The bucket must be PRIVATE: this is the property the whole delivery gate
	// depends on. If an unsigned GET worked, published-only would be theatre.
	unsigned, err := http.Get("http://" + endpoint + "/media/" + key) //nolint:gosec,noctx // fixed test endpoint
	if err != nil {
		t.Fatal(err)
	}
	_ = unsigned.Body.Close()
	if unsigned.StatusCode == http.StatusOK {
		t.Fatal("an unsigned GET returned the object — the bucket is not private")
	}

	// A signed read works and returns the bytes.
	getURL, err := store.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := http.Get(getURL) //nolint:gosec,noctx // signed URL from the store
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET rejected: %d", got.StatusCode)
	}
	data, _ := io.ReadAll(got.Body)
	if !bytes.Equal(data, body) {
		t.Fatalf("body=%q want %q", data, body)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, key); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}

	// The upload limits must be enforced by the SERVER, not by our own code
	// after the fact. These two cases are the whole reason the upload is a POST
	// policy rather than a presigned PUT: a PUT would accept both.
	t.Run("oversized upload is refused by storage", func(t *testing.T) {
		const k = "tenant-a/too-big"
		up, err := store.PresignPost(ctx, k, time.Minute, objectstore.UploadConstraints{
			MaxBytes: 16, ContentType: "image/png",
		})
		if err != nil {
			t.Fatal(err)
		}
		code, err := postUpload(ctx, up, bytes.Repeat([]byte("x"), 4096), "image/png")
		if err != nil {
			t.Fatal(err)
		}
		if code < 400 {
			t.Fatalf("storage accepted a body over the signed limit: %d", code)
		}
		if _, err := store.Stat(ctx, k); !errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("an over-limit object was stored anyway: %v", err)
		}
	})

	t.Run("mismatched content type is refused by storage", func(t *testing.T) {
		const k = "tenant-a/wrong-type"
		up, err := store.PresignPost(ctx, k, time.Minute, objectstore.UploadConstraints{
			MaxBytes: 1024, ContentType: "image/png",
		})
		if err != nil {
			t.Fatal(err)
		}
		// The form is signed for image/png; the client tries to store HTML.
		code, err := postUpload(ctx, up, []byte("<script>alert(1)</script>"), "text/html")
		if err != nil {
			t.Fatal(err)
		}
		if code < 400 {
			t.Fatalf("storage accepted a content type the policy did not allow: %d", code)
		}
		if _, err := store.Stat(ctx, k); !errors.Is(err, objectstore.ErrNotFound) {
			t.Fatalf("a wrong-typed object was stored anyway: %v", err)
		}
	})

	// A form with no limits must not be signable at all.
	t.Run("unconstrained upload cannot be signed", func(t *testing.T) {
		if _, err := store.PresignPost(ctx, "tenant-a/x", time.Minute, objectstore.UploadConstraints{}); err == nil {
			t.Fatal("signing succeeded with no constraints — the limits would be optional")
		}
	})
}

// postUpload sends a presigned form the way a client would: every signed field
// first, the bytes last in a part named "file". contentType is what the client
// declares, which is not necessarily what the policy allows.
func postUpload(ctx context.Context, up objectstore.PresignedUpload, body []byte, contentType string) (int, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range up.Fields {
		if k == "Content-Type" {
			continue // set explicitly below so a test can deliberately diverge
		}
		if err := w.WriteField(k, v); err != nil {
			return 0, err
		}
	}
	if err := w.WriteField("Content-Type", contentType); err != nil {
		return 0, err
	}
	part, err := w.CreateFormFile("file", "upload.bin")
	if err != nil {
		return 0, err
	}
	if _, err := part.Write(body); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.URL, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
