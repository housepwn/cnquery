package packages

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/pkg/errors"
	"github.com/spf13/afero"

	"github.com/rs/zerolog/log"
	motor "go.mondoo.io/mondoo/motor/motoros"
	"go.mondoo.io/mondoo/motor/motoros/platform"
	"go.mondoo.io/mondoo/motor/motoros/platform/winbuild"
)

type OperatingSystemPkgManager interface {
	Name() string
	Format() string
	List() ([]Package, error)
	Available() (map[string]PackageUpdate, error)
}

// this will find the right package manager for the operating system
func ResolveSystemPkgManager(motor *motor.Motor) (OperatingSystemPkgManager, error) {
	var pm OperatingSystemPkgManager

	platform, err := motor.Platform()
	if err != nil {
		return nil, err
	}

	// TODO: use OS family and select package manager
	switch platform.Name {
	case "manjaro", "arch": // arch family
		pm = &PacmanPkgManager{motor: motor}
	case "ubuntu", "debian", "raspbian", "kali": // debian family
		pm = &DebPkgManager{motor: motor}
	case "redhat", "centos", "fedora", "amzn", "ol", "scientific", "photon": // rhel family
		pm = &RpmPkgManager{motor: motor, platform: &platform}
	case "opensuse", "sles", "opensuse-leap", "opensuse-tumbleweed": // suse handling
		pm = &SusePkgManager{RpmPkgManager{motor: motor, platform: &platform}}
	case "alpine": // alpine family
		pm = &AlpinePkgManager{motor: motor}
	case "mac_os_x": // mac os family
		pm = &MacOSPkgManager{motor: motor}
	case "windows":
		pm = &WinPkgManager{motor: motor}
	case "scratch":
		pm = &ScratchPkgManager{motor: motor}
	default:
		return nil, errors.New("could not detect suitable package manager for platform: " + platform.Name)
	}

	return pm, nil
}

// Debian, Ubuntu
type DebPkgManager struct {
	motor *motor.Motor
}

func (dpm *DebPkgManager) Name() string {
	return "Debian Package Manager"
}

func (dpm *DebPkgManager) Format() string {
	return "deb"
}

func (dpm *DebPkgManager) List() ([]Package, error) {
	fs := dpm.motor.Transport.FS()
	dpkgStatusFile := "/var/lib/dpkg/status"
	dpkgStatusDir := "/var/lib/dpkg/status.d"
	_, fErr := fs.Stat(dpkgStatusFile)
	dStat, dErr := fs.Stat(dpkgStatusDir)

	if fErr != nil && dErr != nil {
		log.Debug().Err(fErr).Str("path", dpkgStatusFile).Msg("cannot find status file")
		log.Debug().Err(dErr).Str("path", dpkgStatusDir).Msg("cannot find status dir")
		return nil, fmt.Errorf("could not find dpkg package list")
	}

	pkgList := []Package{}
	// main pkg file for debian systems
	if fErr == nil {
		log.Debug().Str("file", dpkgStatusFile).Msg("parse dpkg status file")
		fi, err := dpm.motor.Transport.File(dpkgStatusFile)
		if err != nil {
			return nil, fmt.Errorf("could not read dpkg package list")
		}
		defer fi.Close()

		list, err := ParseDpkgPackages(fi)
		if err != nil {
			return nil, fmt.Errorf("could not parse dpkg package list")
		}
		pkgList = append(pkgList, list...)
	}

	// e.g. google distroless images stores their pkg data in /var/lib/dpkg/status.d/
	if dErr == nil && dStat.IsDir() == true {
		afutil := afero.Afero{Fs: fs}
		err := afutil.Walk(dpkgStatusDir, func(path string, f os.FileInfo, err error) error {
			if f == nil || f.IsDir() {
				return nil
			}

			log.Debug().Str("path", path).Msg("walk file")
			fi, err := dpm.motor.Transport.File(path)
			if err != nil {
				log.Debug().Err(err).Str("path", path).Msg("could open file")
				return fmt.Errorf("could not read dpkg package list")
			}

			list, err := ParseDpkgPackages(fi)
			fi.Close()
			if err != nil {
				log.Debug().Err(err).Str("path", path).Msg("could not parse")
				return fmt.Errorf("could not parse dpkg package list")
			}

			log.Debug().Int("pkgs", len(list)).Msg("completed parsing")
			pkgList = append(pkgList, list...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return pkgList, nil
}

func (dpm *DebPkgManager) Available() (map[string]PackageUpdate, error) {
	// TODO: run this as a complete shell script in motor
	// DEBIAN_FRONTEND=noninteractive apt-get update >/dev/null 2>&1
	// readlock() { cat /proc/locks | awk '{print $5}' | grep -v ^0 | xargs -I {1} find /proc/{1}/fd -maxdepth 1 -exec readlink {} \; | grep '^/var/lib/dpkg/lock$'; }
	// while test -n "$(readlock)"; do sleep 1; done
	// DEBIAN_FRONTEND=noninteractive apt-get upgrade --dry-run
	dpm.motor.Transport.RunCommand("DEBIAN_FRONTEND=noninteractive apt-get update >/dev/null 2>&1")

	cmd, err := dpm.motor.Transport.RunCommand("DEBIAN_FRONTEND=noninteractive apt-get upgrade --dry-run")
	if err != nil {
		log.Debug().Err(err).Msg("lumi[packages]> could not read package updates")
		return nil, fmt.Errorf("could not read package update list")
	}
	return ParseDpkgUpdates(cmd.Stdout)
}

// RpmPkgManager is the pacakge manager for Redhat, CentOS, Oracle and Suse
// it support two modes: runtime where the rpm command is available and static analysis for images (e.g. container tar)
// If the RpmPkgManager is used in static mode, it extracts the rpm database from the system and copies it to the local
// filesystem to run a local rpm command to extract the data. The static analysis is always slower than using the running
// one since more data need to copied. Therefore the runtime check should be preferred over the static analysis
type RpmPkgManager struct {
	motor         *motor.Motor
	platform      *platform.PlatformInfo
	staticChecked bool
	static        bool
}

func (rpm *RpmPkgManager) Name() string {
	return "Rpm Package Manager"
}

func (rpm *RpmPkgManager) Format() string {
	return "rpm"
}

// determine if we running against a static image, where we cannot execute the rpm command
// once executed, it caches its result to prevent the execution of the checks many times
func (rpm *RpmPkgManager) isStaticAnalysis() bool {
	if rpm.staticChecked == true {
		return rpm.static
	}

	rpm.static = false

	// check if the rpm command exists, e.g it is not available on tar backend
	c, err := rpm.motor.Transport.RunCommand("command -v rpm")
	if err != nil || c.ExitStatus != 0 {
		log.Debug().Msg("lumi[packages]> fallback to static rpm package manager")
		rpm.static = true
	}

	rpm.staticChecked = true
	return rpm.static
}

func (rpm *RpmPkgManager) List() ([]Package, error) {
	if rpm.isStaticAnalysis() {
		return rpm.staticList()
	} else {
		return rpm.runtimeList()
	}
}

func (rpm *RpmPkgManager) Available() (map[string]PackageUpdate, error) {
	if rpm.isStaticAnalysis() {
		return rpm.staticAvailable()
	} else {
		return rpm.runtimeAvailable()
	}
}

func (rpm *RpmPkgManager) queryFormat() string {
	// this format should work everywhere
	// fall-back to epoch instead of epochnum for 6 ish platforms, latest 6 platforms also support epochnum, but we
	// save 1 call by not detecting the available keyword via rpm --querytags
	format := "%{NAME} %{EPOCH}:%{VERSION}-%{RELEASE} %{ARCH} %{SUMMARY}\\n"

	// ATTENTION: EPOCHNUM is only available since later version of rpm in RedHat 6 and Suse 12
	// we can only expect if for rhel 7+, therefore we need to run an extra test
	info, err := rpm.motor.Platform()
	if err != nil {
		return format
	}

	// be aware that this method is also used for non-redhat systems like suse
	i, err := strconv.ParseInt(info.Release, 0, 32)
	if err == nil && (info.Name == "centos" || info.Name == "redhat") && i >= 7 {
		format = "%{NAME} %{EPOCHNUM}:%{VERSION}-%{RELEASE} %{ARCH} %{SUMMARY}\\n"
	}

	return format
}

func (rpm *RpmPkgManager) runtimeList() ([]Package, error) {

	command := fmt.Sprintf("rpm -qa --queryformat '%s'", rpm.queryFormat())
	cmd, err := rpm.motor.Transport.RunCommand(command)
	if err != nil {
		return nil, errors.Wrap(err, "could not read package list")
	}
	return ParseRpmPackages(cmd.Stdout), nil
}

// fetch all available packages, is that working with centos 6?
func (rpm *RpmPkgManager) runtimeAvailable() (map[string]PackageUpdate, error) {
	// python script:
	// import sys;sys.path.insert(0, "/usr/share/yum-cli");import cli;list = cli.YumBaseCli().returnPkgLists(["updates"]);
	// print ''.join(["{\"name\":\""+x.name+"\", \"available\":\""+x.evr+"\",\"arch\":\""+x.arch+"\",\"repo\":\""+x.repo.id+"\"}\n" for x in list.updates]);
	script := "python -c 'import sys;sys.path.insert(0, \"/usr/share/yum-cli\");import cli;list = cli.YumBaseCli().returnPkgLists([\"updates\"]);print \"\".join([ \"{\\\"name\\\":\\\"\"+x.name+\"\\\",\\\"available\\\":\\\"\"+x.evr+\"\\\",\\\"arch\\\":\\\"\"+x.arch+\"\\\",\\\"repo\\\":\\\"\"+x.repo.id+\"\\\"}\\n\" for x in list.updates]);'"

	cmd, err := rpm.motor.Transport.RunCommand(script)
	if err != nil {
		log.Debug().Err(err).Msg("lumi[packages]> could not read package updates")
		return nil, errors.Wrap(err, "could not read package update list")
	}
	return ParseRpmUpdates(cmd.Stdout)
}

func (rpm *RpmPkgManager) staticList() ([]Package, error) {
	rpmTmpDir, err := ioutil.TempDir(os.TempDir(), "mondoo-rpmdb")
	if err != nil {
		return nil, errors.Wrap(err, "could not create local temp directory")
	}
	defer os.RemoveAll(rpmTmpDir)

	// fetch rpm database file and store it in local tmp file
	f, err := rpm.motor.Transport.File("/var/lib/rpm/Packages")

	// on opensuse, the directory usr/lib/sysimage/rpm/Packages is used in tar
	if err != nil && rpm.platform != nil && rpm.platform.IsFamily("suse") {
		log.Debug().Msg("fallback to opensuse rpm package location")
		f, err = rpm.motor.Transport.File("/usr/lib/sysimage/rpm/Packages")
	}

	// throw error if we stil couldn't find the packages file
	if err != nil {
		return nil, errors.Wrap(err, "could not fetch rpm package list")
	}

	fWriter, err := os.Create(filepath.Join(rpmTmpDir, "Packages"))
	if err != nil {
		log.Error().Err(err).Msg("lumi[packages]> could not create tmp file for rpm database")
		return nil, errors.Wrap(err, "could not create local temp file")
	}
	_, err = io.Copy(fWriter, f)
	if err != nil {
		log.Error().Err(err).Msg("lumi[packages]> could not copy rpm to tmp file")
		return nil, fmt.Errorf("could not cache rpm package list")
	}

	log.Debug().Str("rpmdb", rpmTmpDir).Msg("cached rpm database locally")

	// call local rpm tool to extract the packages
	c := exec.Command("rpm", "--dbpath", rpmTmpDir, "-qa", "--queryformat", rpm.queryFormat())

	stdoutBuffer := bytes.Buffer{}
	stderrBuffer := bytes.Buffer{}

	c.Stdout = &stdoutBuffer
	c.Stderr = &stderrBuffer

	err = c.Run()
	if err != nil {
		log.Error().Err(err).Msg("lumi[packages]> could not execute rpm locally")
		return nil, errors.Wrap(err, "could not read package list")
	}

	return ParseRpmPackages(&stdoutBuffer), nil
}

// TODO: Available() not implemented for RpmFileSystemManager
// for now this is not an error since we can easily determine available packages
func (rpm *RpmPkgManager) staticAvailable() (map[string]PackageUpdate, error) {
	return map[string]PackageUpdate{}, nil
}

// Suse, overwrites the Centos handler
type SusePkgManager struct {
	RpmPkgManager
}

func (spm *SusePkgManager) Available() (map[string]PackageUpdate, error) {
	cmd, err := spm.motor.Transport.RunCommand("zypper --xmlout list-updates")
	if err != nil {
		log.Debug().Err(err).Msg("lumi[packages]> could not read package updates")
		return nil, fmt.Errorf("could not read package update list")
	}
	return ParseZypperUpdates(cmd.Stdout)
}

// Arch, Manjaro
type PacmanPkgManager struct {
	motor *motor.Motor
}

func (ppm *PacmanPkgManager) Name() string {
	return "Pacman Package Manager"
}

func (ppm *PacmanPkgManager) Format() string {
	return "pacman"
}

func (ppm *PacmanPkgManager) List() ([]Package, error) {
	cmd, err := ppm.motor.Transport.RunCommand("pacman -Q")
	if err != nil {
		return nil, fmt.Errorf("could not read package list")
	}

	return ParsePacmanPackages(cmd.Stdout), nil
}

func (ppm *PacmanPkgManager) Available() (map[string]PackageUpdate, error) {
	return nil, errors.New("Available() not implemented for pacman")
}

// Arch, Manjaro
type AlpinePkgManager struct {
	motor *motor.Motor
}

func (apm *AlpinePkgManager) Name() string {
	return "Alpine Package Manager"
}

func (apm *AlpinePkgManager) Format() string {
	return "apk"
}

func (apm *AlpinePkgManager) List() ([]Package, error) {
	fr, err := apm.motor.Transport.File("/lib/apk/db/installed")
	if err != nil {
		return nil, fmt.Errorf("could not read package list")
	}
	defer fr.Close()

	return ParseApkDbPackages(fr), nil
}

func (apm *AlpinePkgManager) Available() (map[string]PackageUpdate, error) {
	// it only works if apk is updated
	apm.motor.Transport.RunCommand("apk update")

	// determine package updates
	cmd, err := apm.motor.Transport.RunCommand("apk version -v -l '<'")
	if err != nil {
		log.Debug().Err(err).Msg("lumi[packages]> could not read package updates")
		return nil, fmt.Errorf("could not read package update list")
	}
	return ParseApkUpdates(cmd.Stdout)
}

// MacOS
type MacOSPkgManager struct {
	motor *motor.Motor
}

func (mpm *MacOSPkgManager) Name() string {
	return "macOS Package Manager"
}

func (mpm *MacOSPkgManager) Format() string {
	return "macos"
}

func (mpm *MacOSPkgManager) List() ([]Package, error) {
	cmd, err := mpm.motor.Transport.RunCommand("system_profiler SPApplicationsDataType -xml")
	if err != nil {
		return nil, fmt.Errorf("could not read package list")
	}

	return ParseMacOSPackages(cmd.Stdout)
}

func (mpm *MacOSPkgManager) Available() (map[string]PackageUpdate, error) {
	return nil, errors.New("cannot determine available packages for macOS")
}

type WinPkgManager struct {
	motor *motor.Motor
}

func (win *WinPkgManager) Name() string {
	return "Windows Package Manager"
}

func (win *WinPkgManager) Format() string {
	return "win"
}

// returns installed appx packages as well as hot fixes
func (win *WinPkgManager) List() ([]Package, error) {

	pf, err := win.motor.Platform()
	if err != nil {
		return nil, err
	}

	b, err := winbuild.Version(pf.Release)

	pkgs := []Package{}

	// only win 10+ are compatible with app x packages
	if b.Build > 10240 {
		cmd, err := win.motor.Transport.RunCommand(fmt.Sprintf("powershell -c \"%s\"", WINDOWS_QUERY_APPX_PACKAGES))
		if err != nil {
			return nil, fmt.Errorf("could not read package list")
		}
		appxPkgs, err := ParseWindowsAppxPackages(cmd.Stdout)
		if err != nil {
			return nil, fmt.Errorf("could not read appx package list")
		}
		pkgs = append(pkgs, appxPkgs...)
	}

	cmd, err := win.motor.Transport.RunCommand(fmt.Sprintf("powershell -c \"%s\"", WINDOWS_QUERY_HOTFIXES))
	if err != nil {
		return nil, errors.Wrap(err, "could not fetch hotfixes")
	}
	hotfixes, err := ParseWindowsHotfixes(cmd.Stdout)
	if err != nil {
		return nil, errors.Wrapf(err, "could not parse hotfix result")
	}
	pkgs = append(pkgs, hotfixes...)

	return pkgs, nil
}

func (win *WinPkgManager) Available() (map[string]PackageUpdate, error) {
	return map[string]PackageUpdate{}, nil
}

type ScratchPkgManager struct {
	motor *motor.Motor
}

func (dpm *ScratchPkgManager) Name() string {
	return "Scratch Package Manager"
}

func (dpm *ScratchPkgManager) Format() string {
	return "scratch"
}

func (dpm *ScratchPkgManager) List() ([]Package, error) {
	return []Package{}, nil
}

func (dpm *ScratchPkgManager) Available() (map[string]PackageUpdate, error) {
	return map[string]PackageUpdate{}, nil
}
