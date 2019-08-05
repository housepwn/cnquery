package resolver

import (
	"errors"

	"os"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/rs/zerolog/log"
	motorcloud_docker "go.mondoo.io/mondoo/motor/motorcloud/docker"
	"go.mondoo.io/mondoo/motor/motoros/docker/docker_engine"
	"go.mondoo.io/mondoo/motor/motoros/docker/image"
	"go.mondoo.io/mondoo/motor/motoros/docker/snapshot"
	"go.mondoo.io/mondoo/motor/motoros/types"
)

// When we talk about Docker, users think at leasst of 3 different things:
// - container runtime (e.g. docker engine)
// - container image (eg. from docker engine or registry)
// - container snapshot
//
// Docker made a very good job in abstracting the problem away from the user
// so that he normally does not think about the distinction. But we need to
// think about those aspects since all those need a different implementation and
// handling.
//
// The user wants and needs an easy way to point to those endpoints:
//
// # registries
// -t docker://gcr.io/project/image@sha256:label
// -t docker://index.docker.io/project/image:label
//
// # docker daemon
// -t docker://id -> image
// -t docker://id -> container
//
// # local directory
// -t docker:///path/link_to_image_archive.tar -> Docker Image
// -t docker:///path/link_to_image_archive2.tar -> OCI
// -t docker:///path/link_to_container.tar
//
// Therefore, this package will only implement the auto-discovery and
// redirect to specific implementations once the disovery is completed
func ResolveDockerTransport(endpoint *types.Endpoint) (types.Transport, string, error) {
	// 0. check if we have a tar as input
	//    detect if the tar is a container image format -> container image
	//    or a container snapshot format -> container snapshot
	// 1. check if we have a container id
	//    check if the container is running -> docker engine
	//    check if the container is stopped -> container snapshot
	// 3. check if we have an image id -> container image
	// 4. check if we have a descriptor for a registry -> container image

	if endpoint == nil || len(endpoint.Host) == 0 {
		return nil, "", errors.New("no endpoint provided")
	}

	// TODO: check if we are pointing to a local tar file
	log.Debug().Str("docker", endpoint.Host).Msg("try to resolve the container or image source")
	_, err := os.Stat(endpoint.Host)
	if err == nil {
		log.Debug().Msg("found local docker/image file")

		// try to load docker image tarball
		img, err := tarball.ImageFromPath(endpoint.Host, nil)
		if err == nil {
			log.Debug().Msg("detected docker image")
			var identifier string

			rc := mutate.Extract(img)

			transport, err := image.New(rc)
			if err != nil {
				hash, err := img.Digest()
				if err != nil {
					identifier = motorcloud_docker.MondooContainerImageID(hash.String())
				}
			}
			return transport, identifier, err
		} else {
			log.Debug().Msg("detected docker container snapshot")
			transport, err := snapshot.NewFromFile(endpoint.Host)
			return transport, "", err
		}

		// TODO: detect file format
		return nil, "", errors.New("could not find the container reference")
	}

	// could be an image id/name, container id/name or a short reference to an image in docker engine
	ded := NewDockerEngineDiscovery()
	if ded.IsRunning() {
		ci, err := ded.ContainerInfo(endpoint.Host)
		if err == nil {
			if ci.Running {
				log.Debug().Msg("found running container " + ci.ID)
				transport, err := docker_engine.New(ci.ID)
				return transport, motorcloud_docker.MondooContainerID(ci.ID), err
			} else {
				log.Debug().Msg("found stopped container " + ci.ID)
				transport, err := snapshot.NewFromDockerEngine(ci.ID)
				return transport, motorcloud_docker.MondooContainerID(ci.ID), err
			}
		}

		ii, err := ded.ImageInfo(endpoint.Host)
		if err == nil {
			log.Debug().Msg("found docker engine image " + ii.ID)
			img, rc, err := image.LoadFromDockerEngine(ii.ID)
			if err != nil {
				return nil, "", err
			}

			var identifier string
			hash, err := img.Digest()
			if err == nil {
				identifier = motorcloud_docker.MondooContainerImageID(hash.String())
			}

			transport, err := image.New(rc)
			return transport, identifier, err
		}
	}

	// load container image from remote directoryload tar file into backend
	tag, err := name.NewTag(endpoint.Host, name.WeakValidation)
	if err == nil {
		log.Debug().Str("tag", tag.Name()).Msg("found valid container registry reference")

		img, rc, err := image.LoadFromRegistry(tag)
		if err != nil {
			return nil, "", err
		}

		var identifier string
		hash, err := img.Digest()
		if err == nil {
			identifier = motorcloud_docker.MondooContainerImageID(hash.String())
		}

		transport, err := image.New(rc)
		return transport, identifier, err
	} else {
		log.Debug().Str("image", endpoint.Host).Msg("Could not detect a valid repository url")
		return nil, "", err
	}

	// if we reached here, we assume we have a name of an image or container from a registry
	return nil, "", errors.New("could not find the container reference")
}
