package autotag

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"regexp"

	"github.com/gogits/git-module"
	"github.com/hashicorp/go-version"
)

var (
	majorRex   = regexp.MustCompile(`(?i)\[major\]|\#major`)
	minorRex   = regexp.MustCompile(`(?i)\[minor\]|\#minor`)
	patchRex   = regexp.MustCompile(`(?i)\[patch\]|\#patch`)
	versionRex = regexp.MustCompile(`^v([\d]+\.?.*)`)
)

// GitRepoConfig is the configuration needed to create a new *GitRepo.
type GitRepoConfig struct {
	// Repo is the path to the root of the git repository.
	RepoPath string

	// Branch is the name of the git branch to be tracked for tags. This value
	// must be provided.
	Branch string

	// PreReleaseName is the optional string to be appended to a tag being
	// generated (e.g., v.1.2.3-pre) to indicate the pre-release type.
	//
	// You can provide any string you want (that is valid for a Git tag); here
	// are some recommended values:
	//
	// 		* pre(release)
	// 		* alpha
	// 		* beta
	// 		* rc
	PreReleaseName string

	// PreReleaseTimestampLayout is the optional value that's used to append a
	// timestamp to the git tag. The timezone will always be UTC. This value can
	// either be the string `epoch` to be the UNIX epoch, or a Golang time
	// layout string:
	//
	// * https://golang.org/pkg/time/#pkg-constants
	//
	// If PreReleaseName is an empty string, the timestamp will be appended
	// directly to the SemVer tag:
	//
	// 		v1.2.3-1499308568
	//
	// Assuming PreReleaseName is set to `pre`, the timestamp is appended to
	// that value separated by a period (`.`):
	//
	// 		v1.2.3-pre.1499308568
	PreReleaseTimestampLayout string
}

// GitRepo represents a repository we want to run actions against
type GitRepo struct {
	repo *git.Repository

	currentVersion *version.Version
	currentTag     *git.Commit
	newVersion     *version.Version
	branch         string
	branchID       string // commit id of the branch latest commit (where we will apply the tag)

	preReleaseName            string
	preReleaseTimestampLayout string
}

// NewRepo is a constructor for a repo object, parsing the tags that exist
func NewRepo(cfg GitRepoConfig) (*GitRepo, error) {
	if cfg.Branch == "" {
		return nil, fmt.Errorf("must specify a branch")
	}

	gitDirPath, err := generateGitDirPath(cfg.RepoPath)

	if err != nil {
		return nil, err
	}

	log.Println("Opening repo at", gitDirPath)
	repo, err := git.OpenRepository(gitDirPath)
	if err != nil {
		return nil, err
	}

	r := &GitRepo{
		repo:                      repo,
		branch:                    cfg.Branch,
		preReleaseName:            cfg.PreReleaseName,
		preReleaseTimestampLayout: cfg.PreReleaseTimestampLayout,
	}

	err = r.parseTags()
	if err != nil {
		return nil, err
	}

	if err := r.calcVersion(); err != nil {
		return nil, err
	}

	return r, nil
}

func generateGitDirPath(repoPath string) (string, error) {
	absolutePath, err := filepath.Abs(repoPath)

	if err != nil {
		return "", err
	}

	return filepath.Join(absolutePath, ".git"), nil
}

// Parse tags on repo, sort them, and store the most recent revision in the repo object
func (r *GitRepo) parseTags() error {
	log.Println("Parsing repository tags")

	versions := make(map[*version.Version]*git.Commit)

	tags, err := r.repo.GetTags()
	if err != nil {
		return fmt.Errorf("failed to fetch tags: %s", err.Error())
	}

	for tag, commit := range tags {
		v, err := maybeVersionFromTag(commit)
		if err != nil {
			log.Println("skipping non version tag: ", tag)
			continue
		}

		if v == nil {
			log.Println("skipping non version tag: ", tag)
			continue
		}

		c, err := r.repo.GetCommit(commit)
		if err != nil {
			return fmt.Errorf("error reading commit '%s':  %s", commit, err)
		}
		versions[v] = c
	}

	keys := make([]*version.Version, 0, len(versions))
	for key := range versions {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(version.Collection(keys)))

	// loop over the tags and find the last reachable non pre-release tag,
	// because we want to calculate the tag from v1.2.3 not v1.2.4-pre1.`
	for _, version := range keys {
		if len(version.Prerelease()) == 0 {
			r.currentVersion = version
			r.currentTag = versions[version]
			return nil
		}
		log.Printf("skipping pre-release tag version: %s", version.String())
	}

	return fmt.Errorf("no stable (non pre-release) version tags found")

}

func maybeVersionFromTag(tag string) (*version.Version, error) {
	if tag == "" {
		return nil, fmt.Errorf("empty tag not supported")
	}

	ver, vErr := parseVersion(tag)
	if vErr != nil {
		return nil, fmt.Errorf("couldn't parse version %s: %s", tag, vErr)
	}
	return ver, nil
}

// parseVersion returns a version object from a parsed string. This normalizes semver strings, and adds the ability to parse strings with 'v' leader. so that `v1.0.1`->     `1.0.1`  which we need for berkshelf to work
func parseVersion(v string) (*version.Version, error) {
	if versionRex.MatchString(v) {
		m := versionRex.FindStringSubmatch(v)
		if len(m) >= 2 {
			v = m[1]
		}
	}

	nVersion, err := version.NewVersion(v)
	if err != nil && nVersion != nil && len(nVersion.Segments()) >= 1 {
		return nVersion, err
	}
	return nVersion, nil
}

// LatestVersion Reports the Lattest version of the given repo
// TODO:(jnelson) this could be more intelligent, looking for a nil new and reporitng the latest version found if we refactor autobump at some point Mon Sep 14 13:05:49 2015
func (r *GitRepo) LatestVersion() string {
	return fmt.Sprintf("%s", r.newVersion)
}

func (r *GitRepo) retrieveBranchInfo() error {
	id, err := r.repo.GetBranchCommitID(r.branch)
	if err != nil {
		id, err = r.repo.GetRemoteBranchCommitID(r.branch)
		if err != nil {
			return fmt.Errorf("error getting commit for branch %s: %s", r.branch, err.Error())
		}
	}

	r.branchID = id
	return nil
}

func preReleaseVersion(v *version.Version, name, tsLayout string) (*version.Version, error) {
	if len(name) == 0 && len(tsLayout) == 0 {
		return v, nil
	}

	if len(v.Prerelease()) > 0 {
		return nil, errors.New("*version.Version already has a PreRelease value set")
	}

	buf := &bytes.Buffer{}

	if _, err := buf.WriteString(name); err != nil {
		return nil, err
	}

	if len(tsLayout) > 0 {
		// XXX(theckman): if the buffer already has content written to it, add
		// the `.` character as a delimiter. The `+` character was not used as
		// the delimiter because some systems that support version tags do not
		// allow it within the string (looking at you, Docker).
		if buf.Len() > 0 {
			if _, err := buf.WriteString("."); err != nil {
				return nil, err
			}
		}

		var (
			timestamp   string
			currentTime = time.Now().UTC()
		)

		if tsLayout == "epoch" {
			timestamp = strconv.FormatInt(currentTime.Unix(), 10)
		} else {
			timestamp = currentTime.Format(tsLayout)
		}

		if _, err := buf.WriteString(timestamp); err != nil {
			return nil, err
		}
	}

	verStr := fmt.Sprintf("%s-%s", v.String(), buf.String())
	return version.NewVersion(verStr)
}

// calcVersion looks over commits since the last tag, and will apply the version bump needed. It will patch if no other instruction is found
// it populates the repo.newVersion with the new calculated version
func (r *GitRepo) calcVersion() error {
	r.newVersion = r.currentVersion
	if err := r.retrieveBranchInfo(); err != nil {
		return err
	}

	startCommit, err := r.repo.GetBranchCommit(r.branch)
	if err != nil {
		return err
	}

	l, err := r.repo.CommitsBetween(startCommit, r.currentTag)
	if err != nil {
		log.Printf("Error loading history for tag '%s': %s ", r.currentVersion, err.Error())
	}
	log.Printf("Checking commits from %s to %s ", r.branchID, r.currentTag.ID)

	// Sort the commits oldest to newest. Then process each commit for bumper commands.
	for e := l.Back(); e != nil; e = e.Prev() {
		commit := e.Value.(*git.Commit)
		if commit == nil {
			return fmt.Errorf("commit pointed to nil object. This should not happen: %s", e)
		}

		v, nerr := r.parseCommit(commit)
		if nerr != nil {
			log.Fatal(err)
		}

		if v != nil {
			r.newVersion = v
		}

	}

	// if there is no movement on the version from commits, bump patch
	if r.newVersion == r.currentVersion {
		if r.newVersion, err = patchBumper.bump(r.currentVersion); err != nil {
			return err
		}
	}

	// if we want this to be a PreRelease tag, we need to enhance the format a bit
	if len(r.preReleaseName) > 0 || len(r.preReleaseTimestampLayout) > 0 {
		if r.newVersion, err = preReleaseVersion(r.newVersion, r.preReleaseName, r.preReleaseTimestampLayout); err != nil {
			return err
		}
	}

	return nil
}

// AutoTag applies the new version tag thats calculated
func (r *GitRepo) AutoTag() error {
	return r.tagNewVersion()
}

func (r *GitRepo) tagNewVersion() error {
	// TODO:(jnelson) These should be configurable? Mon Sep 14 12:02:52 2015
	tagName := fmt.Sprintf("v%s", r.newVersion.String())

	log.Println("Writing Tag", tagName)
	err := r.repo.CreateTag(tagName, r.branchID)
	if err != nil {
		return fmt.Errorf("error creating tag: %s", err.Error())
	}
	return nil
}

// parseLog looks at HEAD commit see if we want to increment major/minor/patch
func (r *GitRepo) parseCommit(commit *git.Commit) (*version.Version, error) {
	var b bumper
	msg := commit.Message()
	log.Printf("Parsing %s: %s\n", commit.ID, msg)

	if majorRex.MatchString(msg) {
		log.Println("major bump")
		b = majorBumper
	}

	if minorRex.MatchString(msg) {
		log.Println("minor bump")
		b = minorBumper
	}

	if patchRex.MatchString(msg) {
		log.Println("patch bump")
		b = patchBumper
	}

	if b != nil {
		return b.bump(r.currentVersion)
	}

	return nil, nil
}

// MajorBump will bump the version one major rev 1.0.0 -> 2.0.0
func (r *GitRepo) MajorBump() (*version.Version, error) {
	return majorBumper.bump(r.currentVersion)
}

// MinorBump will bump the version one minor rev 1.1.0 -> 1.2.0
func (r *GitRepo) MinorBump() (*version.Version, error) {
	return minorBumper.bump(r.currentVersion)
}

// PatchBump will bump the version one patch rev 1.1.1 -> 1.1.2
func (r *GitRepo) PatchBump() (*version.Version, error) {
	return patchBumper.bump(r.currentVersion)
}
