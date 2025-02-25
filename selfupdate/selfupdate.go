package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/klauspost/compress/zstd"
)

const (
	// holds a timestamp which triggers the next update
	plat = runtime.GOOS + "-" + runtime.GOARCH // ex: linux-amd64
)

var (
	ErrHashMismatch = errors.New("new file hash mismatch after patch")

	defaultHTTPRequester = HTTPRequester{}
)

// Updater is the configuration and runtime data for doing an update.
//
// Example:
//
//	updater := &selfupdate.Updater{
//		CurrentVersion: version,
//		ApiURL:         "http://updates.yourdomain.com/",
//		CmdName:        "myapp", // app name
//	}
//	if updater != nil {
//		go updater.BackgroundRun()
//	}
type Updater struct {
	CurrentVersion string    // Currently running version. `dev` is a special version here and will cause the updater to never update.
	ApiURL         string    // Base URL for API requests (JSON files).
	CmdName        string    // Command name is appended to the ApiURL like http://apiurl/CmdName/. This represents one binary.
	CheckTime      int       // Time in hours before next check
	RandomizeTime  int       // Time in hours to randomize with CheckTime
	Requester      Requester // Optional parameter to override existing HTTP request handler
	Info           struct {
		Version string
	}
	OnSuccessfulUpdate func() // Optional function to run after an update has successfully taken place
}

func (u *Updater) getExecRelativeDir(dir string) string {
	filename, _ := os.Executable()
	path := filepath.Join(filepath.Dir(filename), dir)
	return path
}

func canUpdate() (err error) {
	// get the directory the file exists in
	path, err := os.Executable()
	if err != nil {
		return
	}

	fileDir := filepath.Dir(path)
	fileName := filepath.Base(path)

	// attempt to open a file in the file's directory
	newPath := filepath.Join(fileDir, fmt.Sprintf(".%s.new", fileName))
	fp, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return
	}
	fp.Close()

	_ = os.Remove(newPath)
	return
}

// BackgroundRun starts the update check and apply cycle.
func (u *Updater) BackgroundRun() error {
	if u.WantUpdate() {
		if err := canUpdate(); err != nil {
			// fail
			return err
		}
		if err := u.Update(); err != nil {
			return err
		}
	}
	return nil
}

// WantUpdate returns boolean designating if an update is desired. If the app's version
// is `dev` WantUpdate will return false.
func (u *Updater) WantUpdate() bool {
	if u.CurrentVersion == "dev" {
		return false
	}

	return true
}

// UpdateAvailable checks if update is available and returns version
func (u *Updater) UpdateAvailable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	old, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer old.Close()

	err = u.FetchInfo()
	if err != nil {
		return "", err
	}
	if u.Info.Version == u.CurrentVersion {
		return "", nil
	} else {
		return u.Info.Version, nil
	}
}

// Update initiates the self update process
func (u *Updater) Update() error {
	path, err := os.Executable()
	if err != nil {
		return err
	}

	if resolvedPath, err := filepath.EvalSymlinks(path); err == nil {
		path = resolvedPath
	}

	// go fetch latest updates manifest
	err = u.FetchInfo()
	if err != nil {
		return err
	}

	// we are on the latest version, nothing to do
	if u.Info.Version == u.CurrentVersion {
		return nil
	}

	old, err := os.Open(path)
	if err != nil {
		return err
	}
	defer old.Close()

	bin, err := u.fetchFullBin()
	if err != nil {
		log.Println("update: fetching full binary(fetchFullBin),", err)
		return err
	}

	// close the old binary before installing because on windows
	// it can't be renamed if a handle to the file is still open
	old.Close()

	err, errRecover := fromStream(bytes.NewBuffer(bin))
	if errRecover != nil {
		return fmt.Errorf("update and recovery errors: %q %q", err, errRecover)
	}
	if err != nil {
		return err
	}

	// update was successful, run func if set
	if u.OnSuccessfulUpdate != nil {
		u.OnSuccessfulUpdate()
	}

	return nil
}

// 強制更新
func (u *Updater) UpdateForce() error {
	path, err := os.Executable()
	if err != nil {
		return err
	}

	if resolvedPath, err := filepath.EvalSymlinks(path); err == nil {
		path = resolvedPath
	}

	old, err := os.Open(path)
	if err != nil {
		return err
	}
	defer old.Close()

	bin, err := u.fetchFullBin()
	if err != nil {
		log.Println("update: fetching full binary(fetchFullBin),", err)
		return err
	}

	// close the old binary before installing because on windows
	// it can't be renamed if a handle to the file is still open
	old.Close()

	err, errRecover := fromStream(bytes.NewBuffer(bin))
	if errRecover != nil {
		return fmt.Errorf("update and recovery errors: %q %q", err, errRecover)
	}
	if err != nil {
		return err
	}

	// update was successful, run func if set
	if u.OnSuccessfulUpdate != nil {
		u.OnSuccessfulUpdate()
	}

	return nil
}

func fromStream(updateWith io.Reader) (err error, errRecover error) {
	updatePath, err := os.Executable()
	if err != nil {
		return
	}

	var newBytes []byte
	newBytes, err = io.ReadAll(updateWith)
	if err != nil {
		return
	}

	// get the directory the executable exists in
	updateDir := filepath.Dir(updatePath)
	filename := filepath.Base(updatePath)

	// Copy the contents of of newbinary to a the new executable file
	newPath := filepath.Join(updateDir, fmt.Sprintf(".%s.new", filename))
	fp, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return
	}
	defer fp.Close()
	_, err = io.Copy(fp, bytes.NewReader(newBytes))

	// if we don't call fp.Close(), windows won't let us move the new executable
	// because the file will still be "in use"
	fp.Close()

	// this is where we'll move the executable to so that we can swap in the updated replacement
	oldPath := filepath.Join(updateDir, fmt.Sprintf(".%s.old", filename))

	// delete any existing old exec file - this is necessary on Windows for two reasons:
	// 1. after a successful update, Windows can't remove the .old file because the process is still running
	// 2. windows rename operations fail if the destination file already exists
	_ = os.Remove(oldPath)

	// move the existing executable to a new file in the same directory
	err = os.Rename(updatePath, oldPath)
	if err != nil {
		return
	}

	// move the new exectuable in to become the new program
	err = os.Rename(newPath, updatePath)

	if err != nil {
		// copy unsuccessful
		errRecover = os.Rename(oldPath, updatePath)
	} else {
		// copy successful, remove the old binary
		errRemove := os.Remove(oldPath)

		// windows has trouble with removing old binaries, so hide it instead
		if errRemove != nil {
			_ = hideFile(oldPath)
		}
	}

	return
}

// FetchInfo fetches the update JSON manifest at u.ApiURL/appname/platform.json
// and updates u.Info.
func (u *Updater) FetchInfo() error {
	r, err := u.fetch(u.ApiURL + url.QueryEscape(u.CmdName) + "/" + url.QueryEscape(plat) + ".json")
	if err != nil {
		return err
	}
	defer r.Close()
	err = json.NewDecoder(r).Decode(&u.Info)
	if err != nil {
		return err
	}
	return nil
}

func (u *Updater) fetchFullBin() ([]byte, error) {
	bin, err := u.fetchBin()
	if err != nil {
		return nil, err
	}
	return bin, nil
}

func (u *Updater) fetchBin() ([]byte, error) {
	r, err := u.fetch(u.ApiURL + url.QueryEscape(u.CmdName) + "/" + url.QueryEscape(u.Info.Version) + "/" + url.QueryEscape(plat) + ".zst")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	buf := new(bytes.Buffer)
	gz, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(buf, gz); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (u *Updater) fetch(url string) (io.ReadCloser, error) {
	if u.Requester == nil {
		return defaultHTTPRequester.Fetch(url)
	}

	readCloser, err := u.Requester.Fetch(url)
	if err != nil {
		return nil, err
	}

	if readCloser == nil {
		return nil, fmt.Errorf("Fetch was expected to return non-nil ReadCloser")
	}

	return readCloser, nil
}
