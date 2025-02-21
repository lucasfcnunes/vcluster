package oci

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	remotev1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/pflag"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	orasremote "oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	// ArtifactType is the vCluster artifact type
	ArtifactType = "application/vnd.loft.vcluster"

	// EtcdLayerMediaType is the reserved media type for the etcd snapshot
	EtcdLayerMediaType = "application/vnd.loft.vcluster.etcd.v1.tar+gzip"
)

type Options struct {
	Repository string `json:"repository,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`

	SkipClientCredentials bool `json:"skip-client-credentials,omitempty"`
}

func (o *Options) FillCredentials(isClient bool) {
	// try to get username and password if not set
	if (isClient && o.SkipClientCredentials) || o.Repository == "" || o.Username != "" {
		return
	}

	// try to get credentials
	ref, err := name.ParseReference(o.Repository)
	if err == nil {
		credentials, _ := GetAuthConfig(ref.Context().RegistryStr())
		if credentials != nil {
			o.Username = credentials.Username
			o.Password = credentials.Secret
		}
	}
}

func AddFlags(fs *pflag.FlagSet, options *Options) {
	// file options
	fs.StringVar(&options.Repository, "oci-repository", options.Repository, "The repository of the snapshot. E.g. ghcr.io/my-user/my-repo")
	fs.StringVar(&options.Username, "oci-username", options.Username, "The username to authenticate with")
	fs.StringVar(&options.Password, "oci-password", options.Password, "The password to authenticate with")
	fs.BoolVar(&options.SkipClientCredentials, "oci-skip-client-credentials", options.SkipClientCredentials, "If true will not try to use the local oci credentials")
}

func NewStore(options *Options) *Store {
	// fill credentials if not set
	options.FillCredentials(false)

	return &Store{
		options: options,
	}
}

type Store struct {
	options *Options
}

func (s *Store) Target() string {
	return "oci://" + s.options.Repository
}

func (s *Store) PutObject(ctx context.Context, body io.Reader) error {
	ref, err := name.ParseReference(s.options.Repository)
	if err != nil {
		return fmt.Errorf("parse repository: %w", err)
	}

	// write to file before pushing
	tempFile, err := writeTempFile(body)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tempFile)

	// make sure it is a tag
	tagRef, ok := ref.(name.Tag)
	if !ok {
		return fmt.Errorf("repository does not have a tag: %v", s.options.Repository)
	}

	// get the relevant info
	registry := tagRef.RegistryStr()
	repository := tagRef.RepositoryStr()
	tag := tagRef.TagStr()

	// create a file store
	fs, err := file.New("/tmp/")
	if err != nil {
		return err
	}
	defer fs.Close()

	// create descriptor array
	descriptors := []v1.Descriptor{}

	// add etcd layer
	etcdDescriptor, err := fs.Add(ctx, "etcd", EtcdLayerMediaType, tempFile)
	if err != nil {
		return fmt.Errorf("add etcd snapshot to image: %w", err)
	}
	descriptors = append(descriptors, etcdDescriptor)

	// pack the files and tag the packed manifest
	manifestDescriptor, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers: descriptors,
	})
	if err != nil {
		return fmt.Errorf("pack vCluster: %w", err)
	}

	// tag the image
	if err = fs.Tag(ctx, manifestDescriptor, tag); err != nil {
		return fmt.Errorf("tag vCluster: %w", err)
	}

	// create client
	repo, err := createClient(registry, repository, s.options.Username, s.options.Password)
	if err != nil {
		return err
	}

	// copy from the file store to the remote repository
	_, err = oras.Copy(ctx, fs, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("push vCluster image: %w", err)
	}

	return nil
}

func (s *Store) GetObject(ctx context.Context) (io.ReadCloser, error) {
	ref, err := name.ParseReference(s.options.Repository)
	if err != nil {
		return nil, err
	}

	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(&authn.Basic{
		Username: s.options.Username,
		Password: s.options.Password,
	}))
	if err != nil {
		return nil, err
	}

	etcdReader, err := FindLayerWithMediaType(img, EtcdLayerMediaType)
	if err != nil {
		return nil, err
	}

	return etcdReader, nil
}

func FindLayerWithMediaType(img remotev1.Image, mediaType string) (io.ReadCloser, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}

	// search config layer
	for _, layer := range layers {
		mt, err := layer.MediaType()
		if err != nil {
			return nil, fmt.Errorf("get layer: %w", err)
		}

		// is config layer?
		if mediaType == string(mt) {
			reader, err := layer.Compressed()
			if err != nil {
				return nil, fmt.Errorf("read config layer: %w", err)
			}

			return reader, nil
		}
	}

	return nil, fmt.Errorf("couldn't find layer with type %s", mediaType)
}

func writeTempFile(reader io.Reader) (string, error) {
	f, err := os.CreateTemp("", "snapshot-")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, reader); err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}

	return f.Name(), nil
}

func createClient(registry, repository, username, password string) (*orasremote.Repository, error) {
	// connect to a remote repository
	repo, err := orasremote.NewRepository(registry + "/" + repository)
	if err != nil {
		return nil, fmt.Errorf("create repository %s/%s: %w", registry, repository, err)
	}

	// Note: The below code can be omitted if authentication is not required
	authClient := &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.DefaultCache,
	}
	if username != "" {
		authClient.Credential = auth.StaticCredential(registry, auth.Credential{
			Username: username,
			Password: password,
		})
	}
	repo.Client = authClient
	return repo, nil
}
