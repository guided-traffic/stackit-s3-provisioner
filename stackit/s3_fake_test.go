package stackit

import (
	"context"
	"testing"

	"github.com/guided-traffic/stackit-s3-provisioner/internal/stackitfake"
)

// newFakeS3Admin starts an in-memory fake, creates a bucket in it and returns
// an S3Admin pointed at the fake's plain-HTTP data plane.
func newFakeS3Admin(t *testing.T, bucket string) (*S3Admin, *stackitfake.Server) {
	t.Helper()
	fake := stackitfake.New(fakeProject, fakeRegion)
	t.Cleanup(fake.Close)
	fake.SeedBucket(bucket, nil)
	s3, err := NewS3Admin(fake.S3.URL, "AKTEST", "SKTEST", fakeRegion)
	if err != nil {
		t.Fatalf("NewS3Admin: %v", err)
	}
	return s3, fake
}

func TestNewS3AdminEndpointSchemes(t *testing.T) {
	for _, endpoint := range []string{
		"object.storage.eu01.onstackit.cloud",         // bare host: TLS assumed
		"https://object.storage.eu01.onstackit.cloud", // explicit https
		"http://127.0.0.1:9000",                       // plain-HTTP test endpoint
	} {
		if _, err := NewS3Admin(endpoint, "ak", "sk", fakeRegion); err != nil {
			t.Errorf("NewS3Admin(%q): %v", endpoint, err)
		}
	}
	if _, err := NewS3Admin("http://bad host with spaces", "ak", "sk", fakeRegion); err == nil {
		t.Error("NewS3Admin(invalid host) succeeded, want error")
	}
}

func TestBucketPolicyRoundtrip(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "pol")

	desired := BuildIsolationPolicy("pol", "urn:admin", "urn:workload")
	if err := s3.SetBucketPolicy(ctx, "pol", desired); err != nil {
		t.Fatalf("SetBucketPolicy: %v", err)
	}
	if got := fake.Policy("pol"); !PoliciesEquivalent(got, desired) {
		t.Errorf("stored policy differs:\ngot  %s\nwant %s", got, desired)
	}
	got, err := s3.GetBucketPolicy(ctx, "pol")
	if err != nil {
		t.Fatalf("GetBucketPolicy: %v", err)
	}
	if !PoliciesEquivalent(got, desired) {
		t.Errorf("GetBucketPolicy differs:\ngot  %s\nwant %s", got, desired)
	}
}

func TestGetBucketPolicyUnset(t *testing.T) {
	s3, _ := newFakeS3Admin(t, "empty-pol")
	got, err := s3.GetBucketPolicy(context.Background(), "empty-pol")
	// minio maps NoSuchBucketPolicy to ("", nil); either way the caller treats
	// the result as "needs to be set".
	if got != "" {
		t.Errorf("GetBucketPolicy(unset) = %q (err %v), want empty", got, err)
	}
}

func TestBucketTagsRoundtrip(t *testing.T) {
	ctx := context.Background()
	s3, _ := newFakeS3Admin(t, "tagged")

	// Untagged bucket reads back as empty map, not an error.
	tags, err := s3.BucketTags(ctx, "tagged")
	if err != nil || len(tags) != 0 {
		t.Fatalf("BucketTags(untagged) = %v, %v; want empty map, nil", tags, err)
	}

	want := map[string]string{"managed-by": "op", "owner": "ns/name"}
	if err := s3.SetBucketTags(ctx, "tagged", want); err != nil {
		t.Fatalf("SetBucketTags: %v", err)
	}
	got, err := s3.BucketTags(ctx, "tagged")
	if err != nil {
		t.Fatalf("BucketTags: %v", err)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestSetBucketTagsInvalid(t *testing.T) {
	s3, _ := newFakeS3Admin(t, "bkt")
	// An empty tag key is rejected client-side by the tags package.
	if err := s3.SetBucketTags(context.Background(), "bkt", map[string]string{"": "v"}); err == nil {
		t.Error("SetBucketTags with empty key succeeded, want error")
	}
}

func TestSetBucketPolicyError(t *testing.T) {
	s3, fake := newFakeS3Admin(t, "bkt")
	fake.FailNext("S3PutPolicy", 403)
	if err := s3.SetBucketPolicy(context.Background(), "bkt", `{"Statement":[]}`); err == nil {
		t.Error("SetBucketPolicy with server failure succeeded, want error")
	}
}

func TestBucketTagsError(t *testing.T) {
	s3, fake := newFakeS3Admin(t, "bkt")
	fake.FailNext("S3GetTagging", 403)
	if _, err := s3.BucketTags(context.Background(), "bkt"); err == nil {
		t.Error("BucketTags with server failure succeeded, want error")
	}
}

func TestBucketEmpty(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "bkt")

	empty, err := s3.BucketEmpty(ctx, "bkt")
	if err != nil || !empty {
		t.Errorf("BucketEmpty(empty bucket) = %v, %v; want true, nil", empty, err)
	}

	fake.SeedObject("bkt", "k1", "v1", false)
	empty, err = s3.BucketEmpty(ctx, "bkt")
	if err != nil || empty {
		t.Errorf("BucketEmpty(non-empty bucket) = %v, %v; want false, nil", empty, err)
	}

	if _, err := s3.BucketEmpty(ctx, "no-such-bucket"); err == nil {
		t.Error("BucketEmpty(missing bucket) succeeded, want error")
	}
}

func TestWipeBucket(t *testing.T) {
	ctx := context.Background()

	t.Run("removes all versions and delete markers", func(t *testing.T) {
		s3, fake := newFakeS3Admin(t, "wipe")
		fake.SeedObject("wipe", "a.txt", "v1", false)
		fake.SeedObject("wipe", "a.txt", "v2", false)
		fake.SeedObject("wipe", "b.txt", "v1", false)
		fake.SeedObject("wipe", "gone.txt", "v9", true) // delete marker

		if err := s3.WipeBucket(ctx, "wipe"); err != nil {
			t.Fatalf("WipeBucket: %v", err)
		}
		if got := fake.ObjectCount("wipe"); got != 0 {
			t.Errorf("ObjectCount after wipe = %d, want 0", got)
		}
		if empty, err := s3.BucketEmpty(ctx, "wipe"); err != nil || !empty {
			t.Errorf("BucketEmpty after wipe = %v, %v; want true", empty, err)
		}
	})

	t.Run("no-op on empty bucket", func(t *testing.T) {
		s3, _ := newFakeS3Admin(t, "wipe")
		if err := s3.WipeBucket(ctx, "wipe"); err != nil {
			t.Fatalf("WipeBucket(empty): %v", err)
		}
	})

	t.Run("listing error is surfaced", func(t *testing.T) {
		s3, fake := newFakeS3Admin(t, "wipe")
		fake.SeedObject("wipe", "a.txt", "v1", false)
		fake.FailNext("S3ListObjects", 403)
		if err := s3.WipeBucket(ctx, "wipe"); err == nil {
			t.Error("WipeBucket with listing failure succeeded, want error")
		}
	})

	t.Run("delete error is surfaced", func(t *testing.T) {
		s3, fake := newFakeS3Admin(t, "wipe")
		fake.SeedObject("wipe", "a.txt", "v1", false)
		fake.FailNext("S3Delete", 403)
		if err := s3.WipeBucket(ctx, "wipe"); err == nil {
			t.Error("WipeBucket with delete failure succeeded, want error")
		}
	})
}
