package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// objectStorage is a complete bucket configuration, written the way the form
// writes it: the prefix keeps its slashes until the signing code strips them.
func objectStorage() Storage {
	return Storage{
		Mode:            StorageModeObject,
		Endpoint:        "https://account.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "boop-uploads",
		Prefix:          "/uploads/",
		PublicURL:       "https://uploads.example.com",
		AccessKeyID:     "an-access-key-id",
		SecretAccessKey: "a-secret-access-key",
	}
}

func TestStorageObjectOptionsAreEmptyOnTheLocalDirectory(t *testing.T) {
	local := Storage{Mode: StorageModeLocal, Bucket: "boop-uploads"}
	if opts := local.ObjectOptions(); opts.Enabled() {
		t.Errorf("a local site still points at a bucket: %+v", opts)
	}
}

func TestStorageObjectOptionsNormaliseThePrefix(t *testing.T) {
	// The form takes the prefix the way documentation writes it, as a path.
	// The signing code addresses keys with the bare form, so the slashes are
	// stripped in one place rather than at every call site.
	for input, want := range map[string]string{"/uploads/": "uploads", "uploads": "uploads", "blog/uploads/": "blog/uploads"} {
		storage := objectStorage()
		storage.Prefix = input
		if got := storage.ObjectOptions().Prefix; got != want {
			t.Errorf("prefix %q became %q, want %q", input, got, want)
		}
	}
}

func TestStorageValidateRefusesAnUnknownMode(t *testing.T) {
	storage := objectStorage()
	storage.Mode = "s3"
	if err := storage.Validate(Credentials{AccessKeyID: true, SecretAccessKey: true}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

func TestStorageValidateRefusesAnObjectStoreWithAFieldMissing(t *testing.T) {
	complete := objectStorage()
	for _, tt := range []struct {
		name        string
		clear       func(*Storage)
		credentials Credentials
		want        string
	}{
		{"endpoint", func(s *Storage) { s.Endpoint = "" }, Credentials{true, true}, "endpoint"},
		{"bucket", func(s *Storage) { s.Bucket = "" }, Credentials{true, true}, "bucket"},
		{"public URL", func(s *Storage) { s.PublicURL = "" }, Credentials{true, true}, "public URL"},
		{"access key", func(s *Storage) {}, Credentials{false, true}, "access key id"},
		{"secret key", func(s *Storage) {}, Credentials{true, false}, "secret access key"},
		{"a malformed endpoint", func(s *Storage) { s.Endpoint = "ftp://example.com" }, Credentials{true, true}, "http"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storage := complete
			tt.clear(&storage)
			err := storage.Validate(tt.credentials)
			if err == nil {
				t.Fatal("an incomplete object store was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to name %q", err, tt.want)
			}
		})
	}

	if err := complete.Validate(Credentials{AccessKeyID: true, SecretAccessKey: true}); err != nil {
		t.Errorf("a complete configuration was refused: %v", err)
	}
}

func TestApplyRefusesAHalfConfiguredObjectStore(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 4)

	for _, tt := range []struct {
		name   string
		update Update
	}{
		{"the mode alone", Update{Values: map[string]any{KeyStorageMode: StorageModeObject}}},
		{"everything but the credentials", Update{Values: map[string]any{
			KeyStorageMode:      StorageModeObject,
			KeyStorageEndpoint:  "https://account.r2.cloudflarestorage.com",
			KeyStorageBucket:    "boop-uploads",
			KeyStoragePublicURL: "https://uploads.example.com",
		}}},
		{"an endpoint that is not a URL", Update{Values: map[string]any{
			KeyStorageEndpoint: "not-a-url",
		}}},
		{"a mode that does not exist", Update{Values: map[string]any{KeyStorageMode: "s3"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := Apply(ctx, db, box, tt.update); !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("Apply = %v, want ErrInvalidValue", err)
			}
		})
	}
}

// A refused merge must leave the database exactly as it was, including the
// fields of the same request that were individually acceptable: a mode written
// without its endpoint would leave the site pointing at nothing.
func TestApplyWritesNothingWhenTheStorageMergeIsRefused(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	err := Apply(ctx, db, testBox(t, 4), Update{Values: map[string]any{
		KeyStorageMode:   StorageModeObject,
		KeyStorageBucket: "boop-uploads",
	}})
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("Apply = %v, want ErrInvalidValue", err)
	}

	values, err := Load(ctx, db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if values.StorageMode != StorageModeLocal {
		t.Errorf("StorageMode = %q, want the documented default", values.StorageMode)
	}
	if values.StorageBucket != "" {
		t.Errorf("StorageBucket = %q, want the refused value to be gone", values.StorageBucket)
	}
	configured, err := StorageConfigured(ctx, db)
	if err != nil {
		t.Fatalf("StorageConfigured: %v", err)
	}
	if configured {
		t.Error("a refused update marked the storage category as saved")
	}
}

func TestApplyAcceptsTheObjectStoreOnceItIsComplete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 4)

	storage := objectStorage()
	if err := Apply(ctx, db, box, Update{
		Values: map[string]any{
			KeyStorageMode:      storage.Mode,
			KeyStorageEndpoint:  storage.Endpoint,
			KeyStorageRegion:    storage.Region,
			KeyStorageBucket:    storage.Bucket,
			KeyStoragePrefix:    storage.Prefix,
			KeyStoragePublicURL: storage.PublicURL,
		},
		Secrets: map[string]string{
			SecretKeyStorageAccessKeyID:     storage.AccessKeyID,
			SecretKeyStorageSecretAccessKey: storage.SecretAccessKey,
		},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := ReadStorage(ctx, db, box, Storage{Mode: StorageModeLocal})
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if got != storage {
		t.Errorf("ReadStorage = %+v, want %+v", got, storage)
	}
}

// Clearing a credential is a write to the storage category, so it is judged
// against the state it produces: a bucket with no key to sign with is not a
// configuration, even though the request itself only deleted a row.
func TestApplyRefusesClearingACredentialTheObjectStoreNeeds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 4)

	if _, err := ImportStorage(ctx, db, box, objectStorage()); err != nil {
		t.Fatalf("ImportStorage: %v", err)
	}
	err := Apply(ctx, db, box, Update{Clear: []string{SecretKeyStorageSecretAccessKey}})
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("Apply = %v, want ErrInvalidValue", err)
	}
	if _, err := ReadSecret(ctx, db, box, SecretKeyStorageSecretAccessKey); err != nil {
		t.Errorf("the credential was deleted anyway: %v", err)
	}

	// Leaving the object store first makes the same request acceptable.
	if err := Apply(ctx, db, box, Update{Values: map[string]any{KeyStorageMode: StorageModeLocal}}); err != nil {
		t.Fatalf("switch to local: %v", err)
	}
	if err := Apply(ctx, db, box, Update{Clear: []string{SecretKeyStorageSecretAccessKey}}); err != nil {
		t.Errorf("clearing a credential of a local site: %v", err)
	}
}

func TestApplyRefusesStorageCredentialsWithoutTheMasterKey(t *testing.T) {
	db := testDB(t)
	if err := Apply(context.Background(), db, nil, Update{Secrets: map[string]string{
		SecretKeyStorageAccessKeyID: "an-access-key-id",
	}}); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("Apply = %v, want ErrMasterKeyRequired", err)
	}
}

// The storage category is not seeded, so an absent row means "follow the
// environment". A save is what takes the environment out of the picture.
func TestReadStorageFollowsTheEnvironmentUntilTheSiteSavesIt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 4)
	environment := objectStorage()

	got, err := ReadStorage(ctx, db, box, environment)
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if got != environment {
		t.Errorf("ReadStorage = %+v, want the environment %+v", got, environment)
	}

	if err := Apply(ctx, db, box, Update{Values: map[string]any{KeyStorageMode: StorageModeLocal}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err = ReadStorage(ctx, db, box, environment)
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if got.Object() {
		t.Errorf("ReadStorage = %+v, want the saved local directory to win", got)
	}
}

// A deployment configured only with BOOP_R2_* cannot have its credentials
// encrypted without BOOP_MASTER_KEY. Nothing may be written in that case: a
// seeded "local" row would quietly move new uploads to the disk while the
// site's published images live in a bucket.
func TestImportStorageNeedsTheMasterKeyAndWritesOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	box := testBox(t, 4)
	environment := objectStorage()

	if _, err := ImportStorage(ctx, db, nil, environment); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("ImportStorage without a box = %v, want ErrMasterKeyRequired", err)
	}
	configured, err := StorageConfigured(ctx, db)
	if err != nil {
		t.Fatalf("StorageConfigured: %v", err)
	}
	if configured {
		t.Fatal("a failed import marked the storage category as saved")
	}
	if got, err := ReadStorage(ctx, db, nil, environment); err != nil || got != environment {
		t.Errorf("ReadStorage = %+v, %v, want the environment to stay in charge", got, err)
	}

	imported, err := ImportStorage(ctx, db, box, environment)
	if err != nil {
		t.Fatalf("ImportStorage: %v", err)
	}
	if !imported {
		t.Fatal("the first import wrote nothing")
	}
	got, err := ReadStorage(ctx, db, box, Storage{Mode: StorageModeLocal})
	if err != nil {
		t.Fatalf("ReadStorage: %v", err)
	}
	if got != environment {
		t.Errorf("ReadStorage = %+v, want the imported %+v", got, environment)
	}

	// A second import must not touch a saved configuration, whatever the
	// environment now says.
	imported, err = ImportStorage(ctx, db, box, Storage{Mode: StorageModeLocal})
	if err != nil {
		t.Fatalf("second ImportStorage: %v", err)
	}
	if imported {
		t.Error("the import overwrote a configuration the site had saved")
	}
	if got, err := ReadStorage(ctx, db, box, Storage{}); err != nil || got != environment {
		t.Errorf("ReadStorage = %+v, %v, want the imported configuration to stay", got, err)
	}
}
