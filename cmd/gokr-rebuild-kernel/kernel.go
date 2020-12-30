package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM debian:stretch

RUN apt-get update && apt-get install -y crossbuild-essential-arm64 bc libssl-dev bison flex

COPY gokr-build-kernel /usr/bin/gokr-build-kernel
COPY {{ .KernelTar }} /var/cache/{{ .KernelTar }}
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src

USER builduser
WORKDIR /usr/src
ENTRYPOINT /usr/bin/gokr-build-kernel
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

var patchFiles = []string{
	"0001-Revert-add-index-to-the-ethernet-alias.patch",
	// serial
	"0101-expose-UART0-ttyAMA0-on-GPIO-14-15-disable-UART1-tty.patch",
	"0102-expose-UART0-ttyAMA0-on-GPIO-14-15-disable-UART1-tty.patch",
	"0103-expose-UART0-ttyAMA0-on-GPIO-14-15-disable-UART1-tty.patch",
	// spi
	"0201-enable-spidev.patch",
}

func copyFile(dest, src string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
}

var gopath = mustGetGopath()

func mustGetGopath() string {
	gopathb, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		log.Panic(err)
	}
	return strings.TrimSpace(string(gopathb))
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	path := filepath.Join(gopath, "src", "github.com", "gokrazy", "kernel", filename)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("could not find file %q (looked in . and %s)", filename, path)
}

func getContainerExecutable() (string, error) {
	// Probe podman first, because the docker binary might actually
	// be a thin podman wrapper with podman behavior.
	choices := []string{"podman", "docker"}
	for _, exe := range choices {
		p, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}
	return "", fmt.Errorf("none of %v found in $PATH", choices)
}

// TODO: remove downloadKernel from ../gokr-build-kernel/build.go if we end up
// always downloading outside the container.
func downloadKernel(destdir, latest string) error {
	out, err := os.Create(filepath.Join(destdir, filepath.Base(latest)))
	if err != nil {
		return err
	}
	defer out.Close()
	resp, err := http.Get(latest)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("unexpected HTTP status code for %s: got %d, want %d", latest, got, want)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Close()
}

func main() {
	executable, err := getContainerExecutable()
	if err != nil {
		log.Fatal(err)
	}
	execName := filepath.Base(executable)
	// We explicitly use /tmp, because Docker only allows volume mounts under
	// certain paths on certain platforms, see
	// e.g. https://docs.docker.com/docker-for-mac/osxfs/#namespaces for macOS.
	tmp, err := ioutil.TempDir("/tmp", "gokr-rebuild-kernel")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command("go", "install", "github.com/gokrazy/kernel/cmd/gokr-build-kernel")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOBIN="+tmp)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%v: %v", cmd.Args, err)
	}

	buildPath := filepath.Join(tmp, "gokr-build-kernel")

	var patchPaths []string
	for _, filename := range patchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	kernelPath, err := find("vmlinuz")
	if err != nil {
		log.Fatal(err)
	}
	dtbPath, err := find("bcm2710-rpi-3-b.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbPlusPath, err := find("bcm2710-rpi-3-b-plus.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbCM3Path, err := find("bcm2710-rpi-cm3.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtb4Path, err := find("bcm2711-rpi-4-b.dtb")
	if err != nil {
		log.Fatal(err)
	}

	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err := copyFile(filepath.Join(tmp, filepath.Base(path)), path); err != nil {
			log.Fatal(err)
		}
	}

	// Download the kernel sources outside of the container, as network inside
	// the container is broken on GitHub actions.
	buildGoPath, err := find("cmd/gokr-build-kernel/build.go")
	if err != nil {
		log.Fatal(err)
	}
	b, err := ioutil.ReadFile(buildGoPath)
	if err != nil {
		log.Fatal(err)
	}
	kernelURLRe := regexp.MustCompile(`var latest = "([^"]+)"`)
	matches := kernelURLRe.FindStringSubmatch(string(b))
	if matches == nil {
		log.Fatalf("regexp %v resulted in no matches", kernelURLRe)
	}

	log.Printf("downloading %s", filepath.Base(matches[1]))
	if err := downloadKernel(tmp, matches[1]); err != nil {
		log.Fatal(err)
	}

	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	dockerFile, err := os.Create(filepath.Join(tmp, "Dockerfile"))
	if err != nil {
		log.Fatal(err)
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid       string
		Gid       string
		BuildPath string
		Patches   []string
		KernelTar string
	}{
		Uid:       u.Uid,
		Gid:       u.Gid,
		BuildPath: buildPath,
		Patches:   patchFiles,
		KernelTar: filepath.Base(matches[1]),
	}); err != nil {
		log.Fatal(err)
	}

	if err := dockerFile.Close(); err != nil {
		log.Fatal(err)
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Dir = tmp
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	if err := dockerBuild.Run(); err != nil {
		log.Fatalf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	var dockerRun *exec.Cmd
	if execName == "podman" {
		dockerRun = exec.Command(executable,
			"run",
			"--userns=keep-id",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	} else {
		dockerRun = exec.Command(executable,
			"run",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	}
	dockerRun.Dir = tmp
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	if err := dockerRun.Run(); err != nil {
		log.Fatalf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, filepath.Join(tmp, "vmlinuz")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPath, filepath.Join(tmp, "bcm2710-rpi-3-b.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPlusPath, filepath.Join(tmp, "bcm2710-rpi-3-b-plus.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbCM3Path, filepath.Join(tmp, "bcm2710-rpi-cm3.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtb4Path, filepath.Join(tmp, "bcm2711-rpi-4-b.dtb")); err != nil {
		log.Fatal(err)
	}

}
