package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/docker/docker/api/types"
	dcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"

	"github.com/buildpacks/pack/internal/archive"
	"github.com/buildpacks/pack/internal/builder"
	"github.com/buildpacks/pack/internal/container"
)

type ContainerOperation func(ctrClient client.CommonAPIClient, ctx context.Context, containerID string, stdout, stderr io.Writer) error

// CopyDir copies a local directory (src) to the destination on the container while filtering files and changing it's UID/GID.
func CopyDir(src, dst string, uid, gid int, fileFilter func(string) bool) ContainerOperation {
	return func(ctrClient client.CommonAPIClient, ctx context.Context, containerID string, stdout, stderr io.Writer) error {
		info, err := ctrClient.Info(ctx)
		if err != nil {
			return err
		}
		if info.OSType == "windows" {
			reader, err := createReader(src, winPathToTarPath(dst), uid, gid, fileFilter)
			if err != nil {
				return errors.Wrapf(err, "create tar archive from '%s'", src)
			}
			defer reader.Close()
			return copyWindows(ctx, ctrClient, containerID, reader, dst, stdout, stderr)
		}
		reader, err := createReader(src, dst, uid, gid, fileFilter)
		if err != nil {
			return errors.Wrapf(err, "create tar archive from '%s'", src)
		}
		defer reader.Close()
		return copyDir(ctx, ctrClient, containerID, reader)
	}
}

func copyDir(ctx context.Context, ctrClient client.CommonAPIClient, containerID string, appReader io.Reader) error {
	var clientErr, err error

	doneChan := make(chan interface{})
	pr, pw := io.Pipe()
	go func() {
		clientErr = ctrClient.CopyToContainer(ctx, containerID, "/", pr, types.CopyToContainerOptions{})
		close(doneChan)
	}()
	func() {
		defer pw.Close()
		_, err = io.Copy(pw, appReader)
	}()

	<-doneChan
	if err == nil {
		err = clientErr
	}

	return err
}

// copyWindows provides an alternate, Windows container-specific implementation of copyDir.
// This implementation is needed because copying directly to a mounted volume is currently buggy
// for Windows containers and does not work. Instead, we perform the copy from inside a container
// using xcopy.
// See: https://github.com/moby/moby/issues/40771
func copyWindows(ctx context.Context, ctrClient client.CommonAPIClient, containerID string, reader io.Reader, dst string, stdout, stderr io.Writer) error {
	info, err := ctrClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return err
	}

	fileOrDir := "d"
	findDst := dst
	if strings.HasSuffix(dst, ".toml") {
		fileOrDir = "f"
		pathElements := strings.Split(dst, `\`)
		findDst = strings.Join(pathElements[:len(pathElements)-1], `\`) // parent of file
	}

	mnt, err := findMount(info, findDst)
	if err != nil {
		return err
	}

	ctr, err := ctrClient.ContainerCreate(ctx,
		&dcontainer.Config{
			Image: info.Image,
			Cmd: []string{
				"cmd",
				"/c",
				fmt.Sprintf(`echo %s|xcopy /e /h /y /c /b c:\windows\%s %s`, fileOrDir, dst[3:], dst),
			},
			WorkingDir: "/",
			User:       windowsContainerAdmin,
		},
		&dcontainer.HostConfig{
			Binds:     []string{fmt.Sprintf("%s:%s", mnt.Name, mnt.Destination)},
			Isolation: dcontainer.IsolationProcess,
		},
		nil, "",
	)
	if err != nil {
		return errors.Wrapf(err, "creating prep container")
	}
	defer ctrClient.ContainerRemove(context.Background(), ctr.ID, types.ContainerRemoveOptions{Force: true})

	err = ctrClient.CopyToContainer(ctx, ctr.ID, "/windows", reader, types.CopyToContainerOptions{})
	if err != nil {
		return errors.Wrap(err, "copy app to container")
	}

	return container.Run(
		ctx,
		ctrClient,
		ctr.ID,
		ioutil.Discard, // Suppress xcopy output
		stderr,
	)
}

func findMount(info types.ContainerJSON, dst string) (types.MountPoint, error) {
	for _, m := range info.Mounts {
		if m.Destination == dst {
			return m, nil
		}
	}
	return types.MountPoint{}, errors.New("no matching mount found")
}

// WriteStackToml writes a `stack.toml` based on the StackMetadata provided to the destination path.
func WriteStackToml(dstPath string, stack builder.StackMetadata) ContainerOperation {
	return func(ctrClient client.CommonAPIClient, ctx context.Context, containerID string, stdout, stderr io.Writer) error {
		buf := &bytes.Buffer{}
		err := toml.NewEncoder(buf).Encode(stack)
		if err != nil {
			return errors.Wrap(err, "marshaling stack metadata")
		}

		tarBuilder := archive.TarBuilder{}

		info, err := ctrClient.Info(ctx)
		if err != nil {
			return err
		}
		if info.OSType == "windows" {
			tarBuilder.AddFile(winPathToTarPath(dstPath), 0755, archive.NormalizedDateTime, buf.Bytes())
			reader := tarBuilder.Reader(archive.DefaultTarWriterFactory())
			defer reader.Close()
			return copyWindows(ctx, ctrClient, containerID, reader, dstPath, stdout, stderr)
		}

		tarBuilder.AddFile(dstPath, 0755, archive.NormalizedDateTime, buf.Bytes())
		reader := tarBuilder.Reader(archive.DefaultTarWriterFactory())
		defer reader.Close()
		return ctrClient.CopyToContainer(ctx, containerID, "/", reader, types.CopyToContainerOptions{})
	}
}

func winPathToTarPath(path string) string {
	return strings.ReplaceAll(path, `\`, "/")[2:] // strip volume, convert slashes
}

func createReader(src, dst string, uid, gid int, fileFilter func(string) bool) (io.ReadCloser, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}

	if fi.IsDir() {
		var mode int64 = -1
		if runtime.GOOS == "windows" {
			mode = 0777
		}

		return archive.ReadDirAsTar(src, dst, uid, gid, mode, false, fileFilter), nil
	}

	return archive.ReadZipAsTar(src, dst, uid, gid, -1, false, fileFilter), nil
}
