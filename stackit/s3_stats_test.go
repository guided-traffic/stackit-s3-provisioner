package stackit

import (
	"context"
	"testing"
)

func TestBucketStatsCurrentObjectsOnly(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "sized")

	fake.SeedObjectVersion("sized", "a.bin", "v1", false, true, 1500)
	fake.SeedObjectVersion("sized", "b.bin", "v1", false, true, 2500)
	// A superseded version and a delete marker: both occupy billed storage but
	// neither appears in a plain object listing, so neither may be counted here.
	fake.SeedObjectVersion("sized", "a.bin", "v0", false, false, 9000)
	fake.SeedObjectVersion("sized", "gone.bin", "v2", true, true, 0)

	st, err := s3.BucketStats(ctx, "sized", false, 0)
	if err != nil {
		t.Fatalf("BucketStats: %v", err)
	}
	if st.Bytes != 4000 || st.Objects != 2 {
		t.Fatalf("current objects = %d bytes / %d objects, want 4000/2", st.Bytes, st.Objects)
	}
	if st.VersionBytes != 0 || st.VersionObjects != 0 {
		t.Fatalf("version counters = %d/%d, want 0/0 without version counting",
			st.VersionBytes, st.VersionObjects)
	}
	if st.BillableBytes() != 4000 {
		t.Fatalf("BillableBytes = %d, want 4000", st.BillableBytes())
	}
	if st.Truncated {
		t.Fatal("Truncated set without a cap")
	}
}

func TestBucketStatsWithVersions(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "versioned")

	fake.SeedObjectVersion("versioned", "a.bin", "v1", false, true, 1500)
	fake.SeedObjectVersion("versioned", "a.bin", "v0", false, false, 9000)
	fake.SeedObjectVersion("versioned", "gone.bin", "v2", true, true, 0)

	st, err := s3.BucketStats(ctx, "versioned", true, 0)
	if err != nil {
		t.Fatalf("BucketStats: %v", err)
	}
	// Current stays current; the superseded version and the delete marker land
	// in the version counters, which is what makes the total match the invoice.
	if st.Bytes != 1500 || st.Objects != 1 {
		t.Fatalf("current = %d bytes / %d objects, want 1500/1", st.Bytes, st.Objects)
	}
	if st.VersionBytes != 9000 {
		t.Fatalf("VersionBytes = %d, want 9000", st.VersionBytes)
	}
	// The superseded version plus the delete marker.
	if st.VersionObjects != 2 {
		t.Fatalf("VersionObjects = %d, want 2", st.VersionObjects)
	}
	if st.BillableBytes() != 10500 {
		t.Fatalf("BillableBytes = %d, want 10500", st.BillableBytes())
	}
}

func TestBucketStatsCapStopsEarly(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "many")

	for _, k := range []string{"a", "b", "c", "d"} {
		fake.SeedObjectVersion("many", k, "v1", false, true, 100)
	}

	st, err := s3.BucketStats(ctx, "many", false, 2)
	if err != nil {
		t.Fatalf("BucketStats: %v", err)
	}
	if !st.Truncated {
		t.Fatal("want Truncated after hitting the cap")
	}
	// The values are a lower bound, not a wrong number: exactly the entries that
	// were consumed before the cap.
	if st.Objects != 2 || st.Bytes != 200 {
		t.Fatalf("capped stats = %d objects / %d bytes, want 2/200", st.Objects, st.Bytes)
	}
}

func TestBucketStatsEmptyBucket(t *testing.T) {
	ctx := context.Background()
	s3, _ := newFakeS3Admin(t, "vacant")

	st, err := s3.BucketStats(ctx, "vacant", false, 0)
	if err != nil {
		t.Fatalf("BucketStats: %v", err)
	}
	if st.Bytes != 0 || st.Objects != 0 || st.Truncated {
		t.Fatalf("empty bucket stats = %+v, want all zero", st)
	}
}

func TestBucketStatsPropagatesListError(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "denied")
	fake.SeedObjectVersion("denied", "a", "v1", false, true, 1)
	fake.FailNext("S3ListObjects", 403)

	if _, err := s3.BucketStats(ctx, "denied", false, 0); err == nil {
		t.Fatal("BucketStats succeeded despite a listing failure")
	}
}

func TestBucketUsageCountsCurrentObjects(t *testing.T) {
	ctx := context.Background()
	s3, fake := newFakeS3Admin(t, "clonesrc")
	fake.SeedObjectVersion("clonesrc", "a", "v1", false, true, 4096)
	fake.SeedObjectVersion("clonesrc", "a", "v0", false, false, 999)

	// The clone feature's denominator must not include superseded versions:
	// rclone copies current objects only.
	got, err := s3.BucketUsage(ctx, "clonesrc")
	if err != nil {
		t.Fatalf("BucketUsage: %v", err)
	}
	if got != 4096 {
		t.Fatalf("BucketUsage = %d, want 4096", got)
	}
}
