package mcnutils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/docker/machine/libmachine/log"
)

const (
	defaultURL            = "https://api.github.com/repos/boot2docker/boot2docker/releases/latest"
	defaultISOFilename    = "boot2docker.iso"
	defaultVolumeIDOffset = int64(0x8028)
	defaultVolumeIDLength = 32
)

var (
	GithubAPIToken string
)

var (
	errGitHubAPIResponse = errors.New(`Error getting a version tag from the Github API response.
You may be getting rate limited by Github.`)
)

func defaultTimeout(network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

func getClient() *http.Client {
	transport := http.Transport{
		DisableKeepAlives: true,
		Proxy:             http.ProxyFromEnvironment,
		Dial:              defaultTimeout,
	}

	return &http.Client{
		Transport: &transport,
	}
}

func getRequest(apiURL string) (*http.Request, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	if GithubAPIToken != "" {
		req.Header.Add("Authorization", fmt.Sprintf("token %s", GithubAPIToken))
	}

	return req, nil
}

// releaseGetter is a client that gets release information of a product and downloads it.
type releaseGetter interface {
	// filename returns filename of the product.
	filename() string
	// getReleaseTag gets a release tag from the given URL.
	getReleaseTag(apiURL string) (string, error)
	// getReleaseURL gets the latest release download URL from the given URL.
	getReleaseURL(apiURL string) (string, error)
	// download downloads a file from the given dlURL and saves it under dir.
	download(dir, file, dlURL string) error
}

// b2dReleaseGetter implements the releaseGetter interface for getting the release of Boot2Docker.
type b2dReleaseGetter struct {
	isoFilename string
}

func (b *b2dReleaseGetter) filename() string {
	if b == nil {
		return ""
	}
	return b.isoFilename
}

// getReleaseTag gets the release tag of Boot2Docker from apiURL.
func (*b2dReleaseGetter) getReleaseTag(apiURL string) (string, error) {
	if apiURL == "" {
		apiURL = defaultURL
	}

	client := getClient()
	req, err := getRequest(apiURL)
	if err != nil {
		return "", err
	}
	rsp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer rsp.Body.Close()

	var t struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(rsp.Body).Decode(&t); err != nil {
		return "", err
	}
	if t.TagName == "" {
		return "", errGitHubAPIResponse
	}
	return t.TagName, nil
}

// getReleaseURL gets the latest release URL of Boot2Docker.
// FIXME: find or create some other way to get the "latest release" of boot2docker since the GitHub API has a pretty low rate limit on API requests
func (b *b2dReleaseGetter) getReleaseURL(apiURL string) (string, error) {
	if apiURL == "" {
		apiURL = defaultURL
	}

	// match github (enterprise) release urls:
	// https://api.github.com/repos/../../releases/latest or
	// https://some.github.enterprise/api/v3/repos/../../releases/latest
	re := regexp.MustCompile("(https?)://([^/]+)(/api/v3)?/repos/([^/]+)/([^/]+)/releases/latest")
	matches := re.FindStringSubmatch(apiURL)
	if len(matches) != 6 {
		// does not match a github releases api URL
		return apiURL, nil
	}

	scheme, host, org, repo := matches[1], matches[2], matches[4], matches[5]
	if host == "api.github.com" {
		host = "github.com"
	}

	tag, err := b.getReleaseTag(apiURL)
	if err != nil {
		return "", err
	}

	log.Infof("Latest release for %s/%s/%s is %s", host, org, repo, tag)
	url := fmt.Sprintf("%s://%s/%s/%s/releases/download/%s/%s", scheme, host, org, repo, tag, b.isoFilename)
	return url, nil
}

func (*b2dReleaseGetter) download(dir, file, isoURL string) error {
	u, err := url.Parse(isoURL)

	var src io.ReadCloser
	if u.Scheme == "file" || u.Scheme == "" {
		s, err := os.Open(u.Path)
		if err != nil {
			return err
		}

		src = s
	} else {
		client := getClient()
		s, err := client.Get(isoURL)
		if err != nil {
			return err
		}

		src = &ReaderWithProgress{
			ReadCloser:     s.Body,
			out:            os.Stdout,
			expectedLength: s.ContentLength,
		}
	}

	defer src.Close()

	// Download to a temp file first then rename it to avoid partial download.
	f, err := ioutil.TempFile(dir, file+".tmp")
	if err != nil {
		return err
	}

	defer func() {
		if err := removeFileIfExists(f.Name()); err != nil {
			log.Warnf("Error removing file: %s", err)
		}
	}()

	if _, err := io.Copy(f, src); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	// Dest is the final path of the boot2docker.iso file.
	dest := filepath.Join(dir, file)

	// Windows can't rename in place, so remove the old file before
	// renaming the temporary downloaded file.
	if err := removeFileIfExists(dest); err != nil {
		return err
	}

	return os.Rename(f.Name(), dest)
}

// iso is an ISO volume.
type iso interface {
	// path returns the path of the ISO.
	path() string
	// exists reports whether the ISO exists.
	exists() bool
	// version returns version information of the ISO.
	version() (string, error)
}

// b2dISO represents a Boot2Docker ISO. It implements the ISO interface.
type b2dISO struct {
	// path of Boot2Docker ISO
	commonIsoPath string

	// offset and length of ISO volume ID
	// cf. http://serverfault.com/questions/361474/is-there-a-way-to-change-a-iso-files-volume-id-from-the-command-line
	volumeIDOffset int64
	volumeIDLength int
}

func (b *b2dISO) path() string {
	if b == nil {
		return ""
	}
	return b.commonIsoPath
}

func (b *b2dISO) exists() bool {
	if b == nil {
		return false
	}

	_, err := os.Stat(b.commonIsoPath)
	return !os.IsNotExist(err)
}

// version scans the volume ID in b and returns its version tag.
func (b *b2dISO) version() (string, error) {
	if b == nil {
		return "", nil
	}

	iso, err := os.Open(b.commonIsoPath)
	if err != nil {
		return "", err
	}
	defer iso.Close()

	isoMetadata := make([]byte, b.volumeIDLength)
	_, err = iso.ReadAt(isoMetadata, b.volumeIDOffset)
	if err != nil {
		return "", err
	}

	verRegex := regexp.MustCompile(`v\d+\.\d+\.\d+`)
	ver := string(verRegex.Find(isoMetadata))
	log.Debug("local Boot2Docker ISO version: ", ver)
	return ver, nil
}

func removeFileIfExists(name string) error {
	if _, err := os.Stat(name); err == nil {
		if err := os.Remove(name); err != nil {
			return fmt.Errorf("Error removing temporary download file: %s", err)
		}
	}
	return nil
}

type B2dUtils struct {
	releaseGetter
	iso
	storePath    string
	imgCachePath string
}

func NewB2dUtils(storePath string) *B2dUtils {
	imgCachePath := filepath.Join(storePath, "cache")

	return &B2dUtils{
		releaseGetter: &b2dReleaseGetter{isoFilename: defaultISOFilename},
		iso: &b2dISO{
			commonIsoPath:  filepath.Join(imgCachePath, defaultISOFilename),
			volumeIDOffset: defaultVolumeIDOffset,
			volumeIDLength: defaultVolumeIDLength,
		},
		storePath:    storePath,
		imgCachePath: imgCachePath,
	}
}

// DownloadISO downloads boot2docker ISO image for the given tag and save it at dest.
func (b *B2dUtils) DownloadISO(dir, file, isoURL string) error {
	log.Infof("Downloading %s from %s...", b.path(), isoURL)
	return b.download(dir, file, isoURL)
}

type ReaderWithProgress struct {
	io.ReadCloser
	out                io.Writer
	bytesTransferred   int64
	expectedLength     int64
	nextPercentToPrint int64
}

func (r *ReaderWithProgress) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)

	if n > 0 {
		r.bytesTransferred += int64(n)
		percentage := r.bytesTransferred * 100 / r.expectedLength

		for percentage >= r.nextPercentToPrint {
			if r.nextPercentToPrint%10 == 0 {
				fmt.Fprintf(r.out, "%d%%", r.nextPercentToPrint)
			} else if r.nextPercentToPrint%2 == 0 {
				fmt.Fprint(r.out, ".")
			}
			r.nextPercentToPrint += 2
		}
	}

	return n, err
}

func (r *ReaderWithProgress) Close() error {
	fmt.Fprintln(r.out)
	return r.ReadCloser.Close()
}

func (b *B2dUtils) DownloadLatestBoot2Docker(apiURL string) error {
	latestReleaseURL, err := b.getReleaseURL(apiURL)
	if err != nil {
		return err
	}

	return b.DownloadISOFromURL(latestReleaseURL)
}

func (b *B2dUtils) DownloadISOFromURL(latestReleaseURL string) error {
	return b.DownloadISO(b.imgCachePath, b.filename(), latestReleaseURL)
}

func (b *B2dUtils) UpdateISOCache(isoURL string) error {
	// recreate the cache dir if it has been manually deleted
	if _, err := os.Stat(b.imgCachePath); os.IsNotExist(err) {
		log.Infof("Image cache directory does not exist, creating it at %s...", b.imgCachePath)
		if err := os.Mkdir(b.imgCachePath, 0700); err != nil {
			return err
		}
	}

	if isoURL != "" {
		// Non-default B2D are not cached
		return nil
	}

	exists := b.exists()
	if !exists {
		log.Info("No default Boot2Docker ISO found locally, downloading the latest release...")
		return b.DownloadLatestBoot2Docker("")
	}

	latest := b.isLatest()
	if !latest {
		log.Info("Default Boot2Docker ISO is out-of-date, downloading the latest release...")
		return b.DownloadLatestBoot2Docker("")
	}

	return nil
}

func (b *B2dUtils) CopyIsoToMachineDir(isoURL, machineName string) error {
	if err := b.UpdateISOCache(isoURL); err != nil {
		return err
	}

	// TODO: This is a bit off-color.
	machineDir := filepath.Join(b.storePath, "machines", machineName)
	machineIsoPath := filepath.Join(machineDir, b.filename())

	// By default just copy the existing "cached" iso to the machine's directory...
	if isoURL == "" {
		log.Infof("Copying %s to %s...", b.path(), machineIsoPath)
		return CopyFile(b.path(), machineIsoPath)
	}

	// if ISO is specified, check if it matches a github releases url or fallback to a direct download
	downloadURL, err := b.getReleaseURL(isoURL)
	if err != nil {
		return err
	}

	return b.DownloadISO(machineDir, b.filename(), downloadURL)
}

// isLatest checks the latest release tag and
// reports whether the local ISO cache is the latest version.
//
// It returns false if failing to get the local ISO version
// and true if failing to fetch the latest release tag.
func (b *B2dUtils) isLatest() bool {
	localVer, err := b.version()
	if err != nil {
		log.Warn("Unable to get the local Boot2Docker ISO version: ", err)
		return false
	}

	latestVer, err := b.getReleaseTag("")
	if err != nil {
		log.Warn("Unable to get the latest Boot2Docker ISO release version: ", err)
		return true
	}

	return localVer == latestVer
}
