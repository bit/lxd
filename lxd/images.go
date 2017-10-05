package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/osarch"
	"github.com/lxc/lxd/shared/version"

	log "gopkg.in/inconshreveable/log15.v2"
)

/* We only want a single publish running at any one time.
   The CPU and I/O load of publish is such that running multiple ones in
   parallel takes longer than running them serially.

   Additionally, publishing the same container or container snapshot
   twice would lead to storage problem, not to mention a conflict at the
   end for whichever finishes last. */
var imagePublishLock sync.Mutex

func detectCompression(fname string) ([]string, string, error) {
	f, err := os.Open(fname)
	if err != nil {
		return []string{""}, "", err
	}
	defer f.Close()

	// read header parts to detect compression method
	// bz2 - 2 bytes, 'BZ' signature/magic number
	// gz - 2 bytes, 0x1f 0x8b
	// lzma - 6 bytes, { [0x000, 0xE0], '7', 'z', 'X', 'Z', 0x00 } -
	// xy - 6 bytes,  header format { 0xFD, '7', 'z', 'X', 'Z', 0x00 }
	// tar - 263 bytes, trying to get ustar from 257 - 262
	header := make([]byte, 263)
	_, err = f.Read(header)
	if err != nil {
		return []string{""}, "", err
	}

	switch {
	case bytes.Equal(header[0:2], []byte{'B', 'Z'}):
		return []string{"-jxf"}, ".tar.bz2", nil
	case bytes.Equal(header[0:2], []byte{0x1f, 0x8b}):
		return []string{"-zxf"}, ".tar.gz", nil
	case (bytes.Equal(header[1:5], []byte{'7', 'z', 'X', 'Z'}) && header[0] == 0xFD):
		return []string{"-Jxf"}, ".tar.xz", nil
	case (bytes.Equal(header[1:5], []byte{'7', 'z', 'X', 'Z'}) && header[0] != 0xFD):
		return []string{"--lzma", "-xf"}, ".tar.lzma", nil
	case bytes.Equal(header[0:3], []byte{0x5d, 0x00, 0x00}):
		return []string{"--lzma", "-xf"}, ".tar.lzma", nil
	case bytes.Equal(header[257:262], []byte{'u', 's', 't', 'a', 'r'}):
		return []string{"-xf"}, ".tar", nil
	case bytes.Equal(header[0:4], []byte{'h', 's', 'q', 's'}):
		return []string{""}, ".squashfs", nil
	default:
		return []string{""}, "", fmt.Errorf("Unsupported compression")
	}

}

func unpack(file string, path string, sType storageType) error {
	extractArgs, extension, err := detectCompression(file)
	if err != nil {
		return err
	}

	command := ""
	args := []string{}
	if strings.HasPrefix(extension, ".tar") {
		command = "tar"
		if runningInUserns {
			args = append(args, "--wildcards")
			args = append(args, "--exclude=dev/*")
			args = append(args, "--exclude=./dev/*")
			args = append(args, "--exclude=rootfs/dev/*")
			args = append(args, "--exclude=rootfs/./dev/*")
		}
		args = append(args, "-C", path, "--numeric-owner")
		args = append(args, extractArgs...)
		args = append(args, file)
	} else if strings.HasPrefix(extension, ".squashfs") {
		command = "unsquashfs"
		args = append(args, "-f", "-d", path, "-n")

		// Limit unsquashfs chunk size to 10% of memory and up to 256MB (default)
		// When running on a low memory system, also disable multi-processing
		mem, err := deviceTotalMemory()
		mem = mem / 1024 / 1024 / 10
		if err == nil && mem < 256 {
			args = append(args, "-da", fmt.Sprintf("%d", mem), "-fr", fmt.Sprintf("%d", mem), "-p", "1")
		}

		args = append(args, file)
	} else {
		return fmt.Errorf("Unsupported image format: %s", extension)
	}

	output, err := shared.RunCommand(command, args...)
	if err != nil {
		// Check if we ran out of space
		fs := syscall.Statfs_t{}

		err1 := syscall.Statfs(path, &fs)
		if err1 != nil {
			return err1
		}

		// Check if we're running out of space
		if int64(fs.Bfree) < int64(2*fs.Bsize) {
			if sType == storageTypeLvm {
				return fmt.Errorf("Unable to unpack image, run out of disk space (consider increasing storage.lvm_volume_size).")
			} else {
				return fmt.Errorf("Unable to unpack image, run out of disk space.")
			}
		}

		co := output
		logger.Debugf("Unpacking failed")
		logger.Debugf(co)

		// Truncate the output to a single line for inclusion in the error
		// message.  The first line isn't guaranteed to pinpoint the issue,
		// but it's better than nothing and better than a multi-line message.
		return fmt.Errorf("Unpack failed, %s.  %s", err, strings.SplitN(co, "\n", 2)[0])
	}

	return nil
}

func unpackImage(imagefname string, destpath string, sType storageType) error {
	err := unpack(imagefname, destpath, sType)
	if err != nil {
		return err
	}

	rootfsPath := fmt.Sprintf("%s/rootfs", destpath)
	if shared.PathExists(imagefname + ".rootfs") {
		err = os.MkdirAll(rootfsPath, 0755)
		if err != nil {
			return fmt.Errorf("Error creating rootfs directory")
		}

		err = unpack(imagefname+".rootfs", rootfsPath, sType)
		if err != nil {
			return err
		}
	}

	if !shared.PathExists(rootfsPath) {
		return fmt.Errorf("Image is missing a rootfs: %s", imagefname)
	}

	return nil
}

func compressFile(path string, compress string) (string, error) {
	reproducible := []string{"gzip"}

	args := []string{"-c"}
	if shared.StringInSlice(compress, reproducible) {
		args = append(args, "-n")
	}

	args = append(args, path)
	cmd := exec.Command(compress, args...)

	outfile, err := os.Create(path + ".compressed")
	if err != nil {
		return "", err
	}

	defer outfile.Close()
	cmd.Stdout = outfile

	err = cmd.Run()
	if err != nil {
		os.Remove(outfile.Name())
		return "", err
	}

	return outfile.Name(), nil
}

type templateEntry struct {
	When       []string          `yaml:"when"`
	CreateOnly bool              `yaml:"create_only"`
	Template   string            `yaml:"template"`
	Properties map[string]string `yaml:"properties"`
}

type imageMetadata struct {
	Architecture string                    `yaml:"architecture"`
	CreationDate int64                     `yaml:"creation_date"`
	ExpiryDate   int64                     `yaml:"expiry_date"`
	Properties   map[string]string         `yaml:"properties"`
	Templates    map[string]*templateEntry `yaml:"templates"`
}

/*
 * This function takes a container or snapshot from the local image server and
 * exports it as an image.
 */
func imgPostContInfo(d *Daemon, r *http.Request, req api.ImagesPost, builddir string) (*api.Image, error) {
	info := api.Image{}
	info.Properties = map[string]string{}
	name := req.Source.Name
	ctype := req.Source.Type
	if ctype == "" || name == "" {
		return nil, fmt.Errorf("No source provided")
	}

	switch ctype {
	case "snapshot":
		if !shared.IsSnapshot(name) {
			return nil, fmt.Errorf("Not a snapshot")
		}
	case "container":
		if shared.IsSnapshot(name) {
			return nil, fmt.Errorf("This is a snapshot")
		}
	default:
		return nil, fmt.Errorf("Bad type")
	}

	info.Filename = req.Filename
	switch req.Public {
	case true:
		info.Public = true
	case false:
		info.Public = false
	}

	c, err := containerLoadByName(d.State(), d.Storage, name)
	if err != nil {
		return nil, err
	}

	// Build the actual image file
	tarfile, err := ioutil.TempFile(builddir, "lxd_build_tar_")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tarfile.Name())

	if err := c.Export(tarfile, req.Properties); err != nil {
		tarfile.Close()
		return nil, err
	}
	tarfile.Close()

	var compressedPath string
	compress := daemonConfig["images.compression_algorithm"].Get()
	if compress != "none" {
		compressedPath, err = compressFile(tarfile.Name(), compress)
		if err != nil {
			return nil, err
		}
	} else {
		compressedPath = tarfile.Name()
	}
	defer os.Remove(compressedPath)

	sha256 := sha256.New()
	tarf, err := os.Open(compressedPath)
	if err != nil {
		return nil, err
	}

	info.Size, err = io.Copy(sha256, tarf)
	tarf.Close()
	if err != nil {
		return nil, err
	}

	info.Fingerprint = fmt.Sprintf("%x", sha256.Sum(nil))

	_, _, err = db.ImageGet(d.db, info.Fingerprint, false, true)
	if err == nil {
		return nil, fmt.Errorf("The image already exists: %s", info.Fingerprint)
	}

	/* rename the the file to the expected name so our caller can use it */
	finalName := shared.VarPath("images", info.Fingerprint)
	err = shared.FileMove(compressedPath, finalName)
	if err != nil {
		return nil, err
	}

	info.Architecture, _ = osarch.ArchitectureName(c.Architecture())
	info.Properties = req.Properties

	// Create storage entry
	err = d.Storage.ImageCreate(info.Fingerprint)
	if err != nil {
		return nil, err
	}

	// Create the database entry
	err = db.ImageInsert(d.db, info.Fingerprint, info.Filename, info.Size, info.Public, info.AutoUpdate, info.Architecture, info.CreatedAt, info.ExpiresAt, info.Properties)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

func imgPostRemoteInfo(d *Daemon, req api.ImagesPost, op *operation) (*api.Image, error) {
	var err error
	var hash string

	if req.Source.Fingerprint != "" {
		hash = req.Source.Fingerprint
	} else if req.Source.Alias != "" {
		hash = req.Source.Alias
	} else {
		return nil, fmt.Errorf("must specify one of alias or fingerprint for init from image")
	}

	info, err := d.ImageDownload(op, req.Source.Server, req.Source.Protocol, req.Source.Certificate, req.Source.Secret, hash, false, req.AutoUpdate)
	if err != nil {
		return nil, err
	}

	id, info, err := db.ImageGet(d.db, info.Fingerprint, false, true)
	if err != nil {
		return nil, err
	}

	// Allow overriding or adding properties
	for k, v := range req.Properties {
		info.Properties[k] = v
	}

	// Update the DB record if needed
	if req.Public || req.AutoUpdate || req.Filename != "" || len(req.Properties) > 0 {
		err = db.ImageUpdate(d.db, id, req.Filename, info.Size, req.Public, req.AutoUpdate, info.Architecture, info.CreatedAt, info.ExpiresAt, info.Properties)
		if err != nil {
			return nil, err
		}
	}

	return info, nil
}

func imgPostURLInfo(d *Daemon, req api.ImagesPost, op *operation) (*api.Image, error) {
	var err error

	if req.Source.URL == "" {
		return nil, fmt.Errorf("Missing URL")
	}

	myhttp, err := util.HTTPClient("", d.proxy)
	if err != nil {
		return nil, err
	}

	// Resolve the image URL
	head, err := http.NewRequest("HEAD", req.Source.URL, nil)
	if err != nil {
		return nil, err
	}

	architecturesStr := []string{}
	for _, arch := range d.os.Architectures {
		architecturesStr = append(architecturesStr, fmt.Sprintf("%d", arch))
	}

	head.Header.Set("User-Agent", version.UserAgent)
	head.Header.Set("LXD-Server-Architectures", strings.Join(architecturesStr, ", "))
	head.Header.Set("LXD-Server-Version", version.Version)

	raw, err := myhttp.Do(head)
	if err != nil {
		return nil, err
	}

	hash := raw.Header.Get("LXD-Image-Hash")
	if hash == "" {
		return nil, fmt.Errorf("Missing LXD-Image-Hash header")
	}

	url := raw.Header.Get("LXD-Image-URL")
	if url == "" {
		return nil, fmt.Errorf("Missing LXD-Image-URL header")
	}

	// Import the image
	info, err := d.ImageDownload(op, url, "direct", "", "", hash, false, req.AutoUpdate)
	if err != nil {
		return nil, err
	}

	id, info, err := db.ImageGet(d.db, info.Fingerprint, false, false)
	if err != nil {
		return nil, err
	}

	// Allow overriding or adding properties
	for k, v := range req.Properties {
		info.Properties[k] = v
	}

	if req.Public || req.AutoUpdate || req.Filename != "" || len(req.Properties) > 0 {
		err = db.ImageUpdate(d.db, id, req.Filename, info.Size, req.Public, req.AutoUpdate, info.Architecture, info.CreatedAt, info.ExpiresAt, info.Properties)
		if err != nil {
			return nil, err
		}
	}

	return info, nil
}

func getImgPostInfo(d *Daemon, r *http.Request, builddir string, post *os.File) (*api.Image, error) {
	info := api.Image{}
	var imageMeta *imageMetadata
	logger := logging.AddContext(logger.Log, log.Ctx{"function": "getImgPostInfo"})

	public, _ := strconv.Atoi(r.Header.Get("X-LXD-public"))
	info.Public = public == 1
	propHeaders := r.Header[http.CanonicalHeaderKey("X-LXD-properties")]
	ctype, ctypeParams, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		ctype = "application/octet-stream"
	}

	sha256 := sha256.New()
	var size int64

	if ctype == "multipart/form-data" {
		// Create a temporary file for the image tarball
		imageTarf, err := ioutil.TempFile(builddir, "lxd_tar_")
		if err != nil {
			return nil, err
		}
		defer os.Remove(imageTarf.Name())

		// Parse the POST data
		post.Seek(0, 0)
		mr := multipart.NewReader(post, ctypeParams["boundary"])

		// Get the metadata tarball
		part, err := mr.NextPart()
		if err != nil {
			return nil, err
		}

		if part.FormName() != "metadata" {
			return nil, fmt.Errorf("Invalid multipart image")
		}

		size, err = io.Copy(io.MultiWriter(imageTarf, sha256), part)
		info.Size += size

		imageTarf.Close()
		if err != nil {
			logger.Error(
				"Failed to copy the image tarfile",
				log.Ctx{"err": err})
			return nil, err
		}

		// Get the rootfs tarball
		part, err = mr.NextPart()
		if err != nil {
			logger.Error(
				"Failed to get the next part",
				log.Ctx{"err": err})
			return nil, err
		}

		if part.FormName() != "rootfs" {
			logger.Error(
				"Invalid multipart image")

			return nil, fmt.Errorf("Invalid multipart image")
		}

		// Create a temporary file for the rootfs tarball
		rootfsTarf, err := ioutil.TempFile(builddir, "lxd_tar_")
		if err != nil {
			return nil, err
		}
		defer os.Remove(rootfsTarf.Name())

		size, err = io.Copy(io.MultiWriter(rootfsTarf, sha256), part)
		info.Size += size

		rootfsTarf.Close()
		if err != nil {
			logger.Error(
				"Failed to copy the rootfs tarfile",
				log.Ctx{"err": err})
			return nil, err
		}

		info.Filename = part.FileName()
		info.Fingerprint = fmt.Sprintf("%x", sha256.Sum(nil))

		expectedFingerprint := r.Header.Get("X-LXD-fingerprint")
		if expectedFingerprint != "" && info.Fingerprint != expectedFingerprint {
			err = fmt.Errorf("fingerprints don't match, got %s expected %s", info.Fingerprint, expectedFingerprint)
			return nil, err
		}

		imageMeta, err = getImageMetadata(imageTarf.Name())
		if err != nil {
			logger.Error(
				"Failed to get image metadata",
				log.Ctx{"err": err})
			return nil, err
		}

		imgfname := shared.VarPath("images", info.Fingerprint)
		err = shared.FileMove(imageTarf.Name(), imgfname)
		if err != nil {
			logger.Error(
				"Failed to move the image tarfile",
				log.Ctx{
					"err":    err,
					"source": imageTarf.Name(),
					"dest":   imgfname})
			return nil, err
		}

		rootfsfname := shared.VarPath("images", info.Fingerprint+".rootfs")
		err = shared.FileMove(rootfsTarf.Name(), rootfsfname)
		if err != nil {
			logger.Error(
				"Failed to move the rootfs tarfile",
				log.Ctx{
					"err":    err,
					"source": rootfsTarf.Name(),
					"dest":   imgfname})
			return nil, err
		}
	} else {
		post.Seek(0, 0)
		size, err = io.Copy(sha256, post)
		info.Size = size
		logger.Debug("Tar size", log.Ctx{"size": size})
		if err != nil {
			logger.Error(
				"Failed to copy the tarfile",
				log.Ctx{"err": err})
			return nil, err
		}

		info.Filename = r.Header.Get("X-LXD-filename")
		info.Fingerprint = fmt.Sprintf("%x", sha256.Sum(nil))

		expectedFingerprint := r.Header.Get("X-LXD-fingerprint")
		if expectedFingerprint != "" && info.Fingerprint != expectedFingerprint {
			logger.Error(
				"Fingerprints don't match",
				log.Ctx{
					"got":      info.Fingerprint,
					"expected": expectedFingerprint})
			err = fmt.Errorf(
				"fingerprints don't match, got %s expected %s",
				info.Fingerprint,
				expectedFingerprint)
			return nil, err
		}

		imageMeta, err = getImageMetadata(post.Name())
		if err != nil {
			logger.Error(
				"Failed to get image metadata",
				log.Ctx{"err": err})
			return nil, err
		}

		imgfname := shared.VarPath("images", info.Fingerprint)
		err = shared.FileMove(post.Name(), imgfname)
		if err != nil {
			logger.Error(
				"Failed to move the tarfile",
				log.Ctx{
					"err":    err,
					"source": post.Name(),
					"dest":   imgfname})
			return nil, err
		}
	}

	info.Architecture = imageMeta.Architecture
	info.CreatedAt = time.Unix(imageMeta.CreationDate, 0)
	info.ExpiresAt = time.Unix(imageMeta.ExpiryDate, 0)

	info.Properties = imageMeta.Properties
	if len(propHeaders) > 0 {
		for _, ph := range propHeaders {
			p, _ := url.ParseQuery(ph)
			for pkey, pval := range p {
				info.Properties[pkey] = pval[0]
			}
		}
	}

	// Create storage entry
	err = d.Storage.ImageCreate(info.Fingerprint)
	if err != nil {
		return nil, err
	}

	// Check if the image already exists
	exists, err := db.ImageExists(d.db, info.Fingerprint)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("Image with same fingerprint already exists")
	}
	// Create the database entry
	err = db.ImageInsert(d.db, info.Fingerprint, info.Filename, info.Size, info.Public, info.AutoUpdate, info.Architecture, info.CreatedAt, info.ExpiresAt, info.Properties)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

func imagesPost(d *Daemon, r *http.Request) Response {
	var err error

	// create a directory under which we keep everything while building
	builddir, err := ioutil.TempDir(shared.VarPath("images"), "lxd_build_")
	if err != nil {
		return InternalError(err)
	}

	cleanup := func(path string, fd *os.File) {
		if fd != nil {
			fd.Close()
		}

		if err := os.RemoveAll(path); err != nil {
			logger.Debugf("Error deleting temporary directory \"%s\": %s", path, err)
		}
	}

	// Store the post data to disk
	post, err := ioutil.TempFile(builddir, "lxd_post_")
	if err != nil {
		cleanup(builddir, nil)
		return InternalError(err)
	}

	_, err = io.Copy(post, r.Body)
	if err != nil {
		cleanup(builddir, post)
		return InternalError(err)
	}

	// Is this a container request?
	post.Seek(0, 0)
	decoder := json.NewDecoder(post)
	imageUpload := false

	req := api.ImagesPost{}
	err = decoder.Decode(&req)
	if err != nil {
		if r.Header.Get("Content-Type") == "application/json" {
			return BadRequest(err)
		}
		imageUpload = true
	}

	if !imageUpload && !shared.StringInSlice(req.Source.Type, []string{"container", "snapshot", "image", "url"}) {
		cleanup(builddir, post)
		return InternalError(fmt.Errorf("Invalid images JSON"))
	}

	// Begin background operation
	run := func(op *operation) error {
		var err error
		var info *api.Image

		// Setup the cleanup function
		defer cleanup(builddir, post)

		if imageUpload {
			/* Processing image upload */
			info, err = getImgPostInfo(d, r, builddir, post)
		} else {
			if req.Source.Type == "image" {
				/* Processing image copy from remote */
				info, err = imgPostRemoteInfo(d, req, op)
			} else if req.Source.Type == "url" {
				/* Processing image copy from URL */
				info, err = imgPostURLInfo(d, req, op)
			} else {
				/* Processing image creation from container */
				imagePublishLock.Lock()
				info, err = imgPostContInfo(d, r, req, builddir)
				imagePublishLock.Unlock()
			}
		}
		if err != nil {
			return err
		}

		// Set the metadata
		metadata := make(map[string]string)
		metadata["fingerprint"] = info.Fingerprint
		metadata["size"] = strconv.FormatInt(info.Size, 10)
		op.UpdateMetadata(metadata)
		return nil
	}

	op, err := operationCreate(operationClassTask, nil, nil, run, nil, nil)
	if err != nil {
		return InternalError(err)
	}

	return OperationResponse(op)
}

func getImageMetadata(fname string) (*imageMetadata, error) {
	metadataName := "metadata.yaml"

	compressionArgs, _, err := detectCompression(fname)

	if err != nil {
		return nil, fmt.Errorf(
			"detectCompression failed, err='%v', tarfile='%s'",
			err,
			fname)
	}

	args := []string{"-O"}
	args = append(args, compressionArgs...)
	args = append(args, fname, metadataName)

	// read the metadata.yaml
	output, err := shared.RunCommand("tar", args...)

	if err != nil {
		outputLines := strings.Split(output, "\n")
		return nil, fmt.Errorf("Could not extract image %s from tar: %v (%s)", metadataName, err, outputLines[0])
	}

	metadata := imageMetadata{}
	err = yaml.Unmarshal([]byte(output), &metadata)

	if err != nil {
		return nil, fmt.Errorf("Could not parse %s: %v", metadataName, err)
	}

	_, err = osarch.ArchitectureId(metadata.Architecture)
	if err != nil {
		return nil, err
	}

	if metadata.CreationDate == 0 {
		return nil, fmt.Errorf("Missing creation date.")
	}

	return &metadata, nil
}

func doImagesGet(d *Daemon, recursion bool, public bool) (interface{}, error) {
	results, err := db.ImagesGet(d.db, public)
	if err != nil {
		return []string{}, err
	}

	resultString := make([]string, len(results))
	resultMap := make([]*api.Image, len(results))
	i := 0
	for _, name := range results {
		if !recursion {
			url := fmt.Sprintf("/%s/images/%s", version.APIVersion, name)
			resultString[i] = url
		} else {
			image, response := doImageGet(d.db, name, public)
			if response != nil {
				continue
			}
			resultMap[i] = image
		}

		i++
	}

	if !recursion {
		return resultString, nil
	}

	return resultMap, nil
}

func imagesGet(d *Daemon, r *http.Request) Response {
	public := !util.IsTrustedClient(r, d.clientCerts)

	result, err := doImagesGet(d, util.IsRecursionRequest(r), public)
	if err != nil {
		return SmartError(err)
	}
	return SyncResponse(true, result)
}

var imagesCmd = Command{name: "images", post: imagesPost, untrustedGet: true, get: imagesGet}

func autoUpdateImages(d *Daemon) {
	logger.Infof("Updating images")

	images, err := db.ImagesGet(d.db, false)
	if err != nil {
		logger.Error("Unable to retrieve the list of images", log.Ctx{"err": err})
		return
	}

	for _, fingerprint := range images {
		id, info, err := db.ImageGet(d.db, fingerprint, false, true)
		if err != nil {
			logger.Error("Error loading image", log.Ctx{"err": err, "fp": fingerprint})
			continue
		}

		if !info.AutoUpdate {
			continue
		}

		autoUpdateImage(d, nil, id, info)
	}

	logger.Infof("Done updating images")
}

// Update a single image.  The operation can be nil, if no progress tracking is needed.
// Returns whether the image has been updated.
func autoUpdateImage(d *Daemon, op *operation, id int, info *api.Image) error {
	fingerprint := info.Fingerprint
	_, source, err := db.ImageSourceGet(d.db, id)
	if err != nil {
		logger.Error("Error getting source image", log.Ctx{"err": err, "fp": fingerprint})
		return err
	}

	logger.Debug("Processing image", log.Ctx{"fp": fingerprint, "server": source.Server, "protocol": source.Protocol, "alias": source.Alias})

	// Set operation metadata to indicate whether a refresh happened
	setRefreshResult := func(result bool) {
		if op == nil {
			return
		}

		metadata := map[string]interface{}{"refreshed": result}
		op.UpdateMetadata(metadata)
	}

	newInfo, err := d.ImageDownload(op, source.Server, source.Protocol, source.Certificate, "", source.Alias, false, true)
	if err != nil {
		logger.Error("Failed to update the image", log.Ctx{"err": err, "fp": fingerprint})
		return err
	}

	// Image didn't change, nothing to do.
	hash := newInfo.Fingerprint
	if hash == fingerprint {
		setRefreshResult(false)
		return nil
	}

	newId, _, err := db.ImageGet(d.db, hash, false, true)
	if err != nil {
		logger.Error("Error loading image", log.Ctx{"err": err, "fp": hash})
		return err
	}

	if info.Cached {
		err = db.ImageLastAccessInit(d.db, hash)
		if err != nil {
			logger.Error("Error setting cached flag", log.Ctx{"err": err, "fp": hash})
			return err
		}
	}

	err = db.ImageLastAccessUpdate(d.db, hash, info.LastUsedAt)
	if err != nil {
		logger.Error("Error setting last use date", log.Ctx{"err": err, "fp": hash})
		return err
	}

	err = db.ImageAliasesMove(d.db, id, newId)
	if err != nil {
		logger.Error("Error moving aliases", log.Ctx{"err": err, "fp": hash})
		return err
	}

	err = doDeleteImage(d, fingerprint)
	if err != nil {
		logger.Error("Error deleting image", log.Ctx{"err": err, "fp": fingerprint})
	}

	setRefreshResult(true)
	return nil
}

func pruneExpiredImages(d *Daemon) {
	// Get the list of expired images.
	expiry := daemonConfig["images.remote_cache_expiry"].GetInt64()

	// Check if we're supposed to prune something
	if expiry <= 0 {
		return
	}

	logger.Infof("Pruning expired images")
	images, err := db.ImagesGetExpired(d.db, expiry)
	if err != nil {
		logger.Error("Unable to retrieve the list of expired images", log.Ctx{"err": err})
		return
	}

	// Delete them
	for _, fp := range images {
		if err := doDeleteImage(d, fp); err != nil {
			logger.Error("Error deleting image", log.Ctx{"err": err, "fp": fp})
		}
	}

	logger.Infof("Done pruning expired images")
}

func doDeleteImage(d *Daemon, fingerprint string) error {
	id, imgInfo, err := db.ImageGet(d.db, fingerprint, false, false)
	if err != nil {
		return err
	}

	// get storage before deleting images/$fp because we need to
	// look at the path
	s, err := storageForImage(d.State(), d.Storage, imgInfo)
	if err != nil {
		logger.Error("error detecting image storage backend", log.Ctx{"fingerprint": imgInfo.Fingerprint, "err": err})
	} else {
		// Remove the image from storage backend
		if err = s.ImageDelete(imgInfo.Fingerprint); err != nil {
			logger.Error("error deleting the image from storage backend", log.Ctx{"fingerprint": imgInfo.Fingerprint, "err": err})
		}
	}

	// Remove main image file
	fname := shared.VarPath("images", imgInfo.Fingerprint)
	if shared.PathExists(fname) {
		err = os.Remove(fname)
		if err != nil {
			logger.Debugf("Error deleting image file %s: %s", fname, err)
		}
	}

	// Remove the rootfs file
	fname = shared.VarPath("images", imgInfo.Fingerprint) + ".rootfs"
	if shared.PathExists(fname) {
		err = os.Remove(fname)
		if err != nil {
			logger.Debugf("Error deleting image file %s: %s", fname, err)
		}
	}

	// Remove the DB entry
	if err = db.ImageDelete(d.db, id); err != nil {
		return err
	}

	return nil
}

func imageDelete(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]

	rmimg := func(op *operation) error {
		return doDeleteImage(d, fingerprint)
	}

	resources := map[string][]string{}
	resources["images"] = []string{fingerprint}

	op, err := operationCreate(operationClassTask, resources, nil, rmimg, nil, nil)
	if err != nil {
		return InternalError(err)
	}

	return OperationResponse(op)
}

func doImageGet(dbObj *sql.DB, fingerprint string, public bool) (*api.Image, Response) {
	_, imgInfo, err := db.ImageGet(dbObj, fingerprint, public, false)
	if err != nil {
		return nil, SmartError(err)
	}

	return imgInfo, nil
}

func imageValidSecret(fingerprint string, secret string) bool {
	for _, op := range operations {
		if op.resources == nil {
			continue
		}

		opImages, ok := op.resources["images"]
		if !ok {
			continue
		}

		if !shared.StringInSlice(fingerprint, opImages) {
			continue
		}

		opSecret, ok := op.metadata["secret"]
		if !ok {
			continue
		}

		if opSecret == secret {
			// Token is single-use, so cancel it now
			op.Cancel()
			return true
		}
	}

	return false
}

func imageGet(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]
	public := !util.IsTrustedClient(r, d.clientCerts)
	secret := r.FormValue("secret")

	info, response := doImageGet(d.db, fingerprint, false)
	if response != nil {
		return response
	}

	if !info.Public && public && !imageValidSecret(info.Fingerprint, secret) {
		return NotFound
	}

	return SyncResponse(true, info)
}

func imagePut(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]

	req := api.ImagePut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return BadRequest(err)
	}

	id, info, err := db.ImageGet(d.db, fingerprint, false, false)
	if err != nil {
		return SmartError(err)
	}

	err = db.ImageUpdate(d.db, id, info.Filename, info.Size, req.Public, req.AutoUpdate, info.Architecture, info.CreatedAt, info.ExpiresAt, req.Properties)
	if err != nil {
		return SmartError(err)
	}

	return EmptySyncResponse
}

var imageCmd = Command{name: "images/{fingerprint}", untrustedGet: true, get: imageGet, put: imagePut, delete: imageDelete}

func aliasesPost(d *Daemon, r *http.Request) Response {
	req := api.ImageAliasesPost{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return BadRequest(err)
	}

	if req.Name == "" || req.Target == "" {
		return BadRequest(fmt.Errorf("name and target are required"))
	}

	// This is just to see if the alias name already exists.
	_, _, err := db.ImageAliasGet(d.db, req.Name, true)
	if err == nil {
		return Conflict
	}

	id, _, err := db.ImageGet(d.db, req.Target, false, false)
	if err != nil {
		return SmartError(err)
	}

	err = db.ImageAliasAdd(d.db, req.Name, id, req.Description)
	if err != nil {
		return SmartError(err)
	}

	return SyncResponseLocation(true, nil, fmt.Sprintf("/%s/images/aliases/%s", version.APIVersion, req.Name))
}

func aliasesGet(d *Daemon, r *http.Request) Response {
	recursion := util.IsRecursionRequest(r)

	q := "SELECT name FROM images_aliases"
	var name string
	inargs := []interface{}{}
	outfmt := []interface{}{name}
	results, err := db.QueryScan(d.db, q, inargs, outfmt)
	if err != nil {
		return BadRequest(err)
	}
	responseStr := []string{}
	responseMap := []api.ImageAliasesEntry{}
	for _, res := range results {
		name = res[0].(string)
		if !recursion {
			url := fmt.Sprintf("/%s/images/aliases/%s", version.APIVersion, name)
			responseStr = append(responseStr, url)

		} else {
			isTrustedClient := util.IsTrustedClient(r, d.clientCerts)
			_, alias, err := db.ImageAliasGet(d.db, name, isTrustedClient)
			if err != nil {
				continue
			}
			responseMap = append(responseMap, alias)
		}
	}

	if !recursion {
		return SyncResponse(true, responseStr)
	}

	return SyncResponse(true, responseMap)
}

func aliasGet(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]

	_, alias, err := db.ImageAliasGet(d.db, name, util.IsTrustedClient(r, d.clientCerts))
	if err != nil {
		return SmartError(err)
	}

	return SyncResponse(true, alias)
}

func aliasDelete(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]
	_, _, err := db.ImageAliasGet(d.db, name, true)
	if err != nil {
		return SmartError(err)
	}

	err = db.ImageAliasDelete(d.db, name)
	if err != nil {
		return SmartError(err)
	}

	return EmptySyncResponse
}

func aliasPut(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]

	req := api.ImageAliasesEntryPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return BadRequest(err)
	}

	if req.Target == "" {
		return BadRequest(fmt.Errorf("The target field is required"))
	}

	id, _, err := db.ImageAliasGet(d.db, name, true)
	if err != nil {
		return SmartError(err)
	}

	imageId, _, err := db.ImageGet(d.db, req.Target, false, false)
	if err != nil {
		return SmartError(err)
	}

	err = db.ImageAliasUpdate(d.db, id, imageId, req.Description)
	if err != nil {
		return SmartError(err)
	}

	return EmptySyncResponse
}

func aliasPost(d *Daemon, r *http.Request) Response {
	name := mux.Vars(r)["name"]

	req := api.ImageAliasesEntryPost{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return BadRequest(err)
	}

	// Check that the name isn't already in use
	id, _, _ := db.ImageAliasGet(d.db, req.Name, true)
	if id > 0 {
		return Conflict
	}

	id, _, err := db.ImageAliasGet(d.db, name, true)
	if err != nil {
		return SmartError(err)
	}

	err = db.ImageAliasRename(d.db, id, req.Name)
	if err != nil {
		return SmartError(err)
	}

	return SyncResponseLocation(true, nil, fmt.Sprintf("/%s/images/aliases/%s", version.APIVersion, req.Name))
}

func imageExport(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]

	public := !util.IsTrustedClient(r, d.clientCerts)
	secret := r.FormValue("secret")

	_, imgInfo, err := db.ImageGet(d.db, fingerprint, false, false)
	if err != nil {
		return SmartError(err)
	}

	if !imgInfo.Public && public && !imageValidSecret(imgInfo.Fingerprint, secret) {
		return NotFound
	}

	imagePath := shared.VarPath("images", imgInfo.Fingerprint)
	rootfsPath := imagePath + ".rootfs"

	_, ext, err := detectCompression(imagePath)
	if err != nil {
		ext = ""
	}
	filename := fmt.Sprintf("%s%s", imgInfo.Fingerprint, ext)

	if shared.PathExists(rootfsPath) {
		files := make([]fileResponseEntry, 2)

		files[0].identifier = "metadata"
		files[0].path = imagePath
		files[0].filename = "meta-" + filename

		// Recompute the extension for the root filesystem, it may use a different
		// compression algorithm than the metadata.
		_, ext, err = detectCompression(rootfsPath)
		if err != nil {
			ext = ""
		}
		filename = fmt.Sprintf("%s%s", imgInfo.Fingerprint, ext)

		files[1].identifier = "rootfs"
		files[1].path = rootfsPath
		files[1].filename = filename

		return FileResponse(r, files, nil, false)
	}

	files := make([]fileResponseEntry, 1)
	files[0].identifier = filename
	files[0].path = imagePath
	files[0].filename = filename

	return FileResponse(r, files, nil, false)
}

func imageSecret(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]
	_, imgInfo, err := db.ImageGet(d.db, fingerprint, false, false)
	if err != nil {
		return SmartError(err)
	}

	secret, err := shared.RandomCryptoString()

	if err != nil {
		return InternalError(err)
	}

	meta := shared.Jmap{}
	meta["secret"] = secret

	resources := map[string][]string{}
	resources["images"] = []string{imgInfo.Fingerprint}

	op, err := operationCreate(operationClassToken, resources, meta, nil, nil, nil)
	if err != nil {
		return InternalError(err)
	}

	return OperationResponse(op)
}

var imagesExportCmd = Command{name: "images/{fingerprint}/export", untrustedGet: true, get: imageExport}
var imagesSecretCmd = Command{name: "images/{fingerprint}/secret", post: imageSecret}

var aliasesCmd = Command{name: "images/aliases", post: aliasesPost, get: aliasesGet}

var aliasCmd = Command{name: "images/aliases/{name:.*}", untrustedGet: true, get: aliasGet, delete: aliasDelete, put: aliasPut, post: aliasPost}
