// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2017 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/retry.v1"

	"github.com/snapcore/snapd/arch"
	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/sysdb"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/errtracker"
	"github.com/snapcore/snapd/httputil"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/strutil"
)

var (
	// TODO: move inside the repairs themselves?
	defaultRepairTimeout = 30 * time.Minute
)

var errtrackerReportRepair = errtracker.ReportRepair

// Repair is a runnable repair.
type Repair struct {
	*asserts.Repair

	run      *Runner
	sequence int
}

func (r *Repair) RunDir() string {
	return filepath.Join(dirs.SnapRepairRunDir, r.BrandID(), strconv.Itoa(r.RepairID()))
}

// SetStatus sets the status of the repair in the state and saves the latter.
func (r *Repair) SetStatus(status RepairStatus) {
	brandID := r.BrandID()
	cur := *r.run.state.Sequences[brandID][r.sequence-1]
	cur.Status = status
	r.run.setRepairState(brandID, cur)
	r.run.SaveState()
}

// Run executes the repair script leaving execution trail files on disk.
func (r *Repair) Run() error {
	// write the script to disk
	rundir := r.RunDir()
	err := os.MkdirAll(rundir, 0775)
	if err != nil {
		return err
	}

	baseName := fmt.Sprintf("r%d", r.Revision())

	script := filepath.Join(rundir, baseName+".script")
	err = osutil.AtomicWriteFile(script, r.Body(), 0700, 0)
	if err != nil {
		return err
	}

	// the date may be broken so we use an additional counter
	logPath := filepath.Join(rundir, baseName+".output")
	logf, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer logf.Close()

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer statusR.Close()
	defer statusW.Close()

	// run the script
	env := os.Environ()
	// we need to hardcode FD=3 because this is the FD after
	// exec.Command() forked. there is no way in go currently
	// to run something right after fork() in the child to
	// know the fd. However because go will close all fds
	// except the ones in "cmd.ExtraFiles" we are safe to set "3"
	env = append(env, "SNAP_REPAIR_STATUS_FD=3")
	env = append(env, "SNAP_REPAIR_RUN_DIR="+rundir)

	workdir := filepath.Join(rundir, "work")
	if err := os.MkdirAll(workdir, 0700); err != nil {
		return err
	}

	cmd := exec.Command(script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = env
	cmd.Dir = workdir
	cmd.ExtraFiles = []*os.File{statusW}
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err = cmd.Start(); err != nil {
		return err
	}
	statusW.Close()

	// wait for repair to finish or timeout
	var scriptErr error
	killTimerCh := time.After(defaultRepairTimeout)
	doneCh := make(chan error)
	go func() {
		doneCh <- cmd.Wait()
		close(doneCh)
	}()
	select {
	case scriptErr = <-doneCh:
		// done
	case <-killTimerCh:
		if err := osutil.KillProcessGroup(cmd); err != nil {
			logger.Noticef("cannot kill timed out repair %s: %s", r, err)
		}
		scriptErr = fmt.Errorf("repair did not finish within %s", defaultRepairTimeout)
	}
	// read repair status pipe, use the last value
	status := readStatus(statusR)
	statusPath := filepath.Join(rundir, baseName+"."+status.String())

	// if the script had an error exit status still honor what we
	// read from the status-pipe, however report the error
	if scriptErr != nil {
		scriptErr = fmt.Errorf("%q failed: %s", r.Ref(), scriptErr)
		if err := r.errtrackerReport(scriptErr, status, logPath); err != nil {
			logger.Noticef("cannot report error to errtracker: %s", err)
		}
		// ensure the error is present in the output log
		fmt.Fprintf(logf, "\n%s", scriptErr)
	}
	if err := os.Rename(logPath, statusPath); err != nil {
		return err
	}
	r.SetStatus(status)

	return nil
}

func readStatus(r io.Reader) RepairStatus {
	var status RepairStatus
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "done":
			status = DoneStatus
		case "skip":
			status = SkipStatus
		}
	}
	if scanner.Err() != nil {
		return RetryStatus
	}
	return status
}

// errtrackerReport reports an repairErr with the given logPath to the
// snap error tracker.
func (r *Repair) errtrackerReport(repairErr error, status RepairStatus, logPath string) error {
	errMsg := fmt.Sprintf("%s", repairErr)

	scriptOutput, err := ioutil.ReadFile(logPath)
	if err != nil {
		logger.Noticef("cannot read %s", logPath)
	}
	s := fmt.Sprintf("%s/%d", r.BrandID(), r.RepairID())

	dupSig := fmt.Sprintf("%s\n%s\noutput:\n%s", s, errMsg, scriptOutput)
	extra := map[string]string{
		"Revision": strconv.Itoa(r.Revision()),
		"BrandID":  r.BrandID(),
		"RepairID": strconv.Itoa(r.RepairID()),
		"Status":   status.String(),
	}
	_, err = errtrackerReportRepair(s, errMsg, dupSig, extra)
	return err
}

// Runner implements fetching, tracking and running repairs.
type Runner struct {
	BaseURL *url.URL
	cli     *http.Client

	state         state
	stateModified bool

	// sequenceNext keeps track of the next integer id in a brand sequence to considered in this run, see Next.
	sequenceNext map[string]int
}

// NewRunner returns a Runner.
func NewRunner() *Runner {
	run := &Runner{
		sequenceNext: make(map[string]int),
	}
	opts := httputil.ClientOpts{
		MayLogBody: false,
		TLSConfig: &tls.Config{
			Time: run.now,
		},
	}
	run.cli = httputil.NewHTTPClient(&opts)
	return run
}

var (
	fetchRetryStrategy = retry.LimitCount(7, retry.LimitTime(90*time.Second,
		retry.Exponential{
			Initial: 500 * time.Millisecond,
			Factor:  2.5,
		},
	))

	peekRetryStrategy = retry.LimitCount(5, retry.LimitTime(44*time.Second,
		retry.Exponential{
			Initial: 300 * time.Millisecond,
			Factor:  2.5,
		},
	))
)

var (
	ErrRepairNotFound    = errors.New("repair not found")
	ErrRepairNotModified = errors.New("repair was not modified")
)

var (
	maxRepairScriptSize = 24 * 1024 * 1024
)

// Fetch retrieves a stream with the repair with the given ids and any
// auxiliary assertions. If revision>=0 the request will include an
// If-None-Match header with an ETag for the revision, and
// ErrRepairNotModified is returned if the revision is still current.
func (run *Runner) Fetch(brandID string, repairID int, revision int) (repair *asserts.Repair, aux []asserts.Assertion, err error) {
	u, err := run.BaseURL.Parse(fmt.Sprintf("repairs/%s/%d", brandID, repairID))
	if err != nil {
		return nil, nil, err
	}

	var r []asserts.Assertion
	resp, err := httputil.RetryRequest(u.String(), func() (*http.Response, error) {
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/x.ubuntu.assertion")
		if revision >= 0 {
			req.Header.Set("If-None-Match", fmt.Sprintf(`"%d"`, revision))
		}
		return run.cli.Do(req)
	}, func(resp *http.Response) error {
		if resp.StatusCode == 200 {
			// decode assertions
			dec := asserts.NewDecoderWithTypeMaxBodySize(resp.Body, map[*asserts.AssertionType]int{
				asserts.RepairType: maxRepairScriptSize,
			})
			for {
				a, err := dec.Decode()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				r = append(r, a)
			}
			if len(r) == 0 {
				return io.ErrUnexpectedEOF
			}
		}
		return nil
	}, fetchRetryStrategy)

	if err != nil {
		return nil, nil, err
	}

	moveTimeLowerBound := true
	defer func() {
		if moveTimeLowerBound {
			t, _ := http.ParseTime(resp.Header.Get("Date"))
			run.moveTimeLowerBound(t)
		}
	}()

	switch resp.StatusCode {
	case 200:
		// ok
	case 304:
		// not modified
		return nil, nil, ErrRepairNotModified
	case 404:
		return nil, nil, ErrRepairNotFound
	default:
		moveTimeLowerBound = false
		return nil, nil, fmt.Errorf("cannot fetch repair, unexpected status %d", resp.StatusCode)
	}

	repair, aux, err = checkStream(brandID, repairID, r)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot fetch repair, %v", err)
	}

	if repair.Revision() <= revision {
		// this shouldn't happen but if it does we behave like
		// all the rest of assertion infrastructure and ignore
		// the now superseded revision
		return nil, nil, ErrRepairNotModified
	}

	return
}

func checkStream(brandID string, repairID int, r []asserts.Assertion) (repair *asserts.Repair, aux []asserts.Assertion, err error) {
	if len(r) == 0 {
		return nil, nil, fmt.Errorf("empty repair assertions stream")
	}
	var ok bool
	repair, ok = r[0].(*asserts.Repair)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected first assertion %q", r[0].Type().Name)
	}

	if repair.BrandID() != brandID || repair.RepairID() != repairID {
		return nil, nil, fmt.Errorf("repair id mismatch %s/%d != %s/%d", repair.BrandID(), repair.RepairID(), brandID, repairID)
	}

	return repair, r[1:], nil
}

type peekResp struct {
	Headers map[string]interface{} `json:"headers"`
}

// Peek retrieves the headers for the repair with the given ids.
func (run *Runner) Peek(brandID string, repairID int) (headers map[string]interface{}, err error) {
	u, err := run.BaseURL.Parse(fmt.Sprintf("repairs/%s/%d", brandID, repairID))
	if err != nil {
		return nil, err
	}

	var rsp peekResp

	resp, err := httputil.RetryRequest(u.String(), func() (*http.Response, error) {
		req, err := http.NewRequest("GET", u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		return run.cli.Do(req)
	}, func(resp *http.Response) error {
		rsp.Headers = nil
		if resp.StatusCode == 200 {
			dec := json.NewDecoder(resp.Body)
			return dec.Decode(&rsp)
		}
		return nil
	}, peekRetryStrategy)

	if err != nil {
		return nil, err
	}

	moveTimeLowerBound := true
	defer func() {
		if moveTimeLowerBound {
			t, _ := http.ParseTime(resp.Header.Get("Date"))
			run.moveTimeLowerBound(t)
		}
	}()

	switch resp.StatusCode {
	case 200:
		// ok
	case 404:
		return nil, ErrRepairNotFound
	default:
		moveTimeLowerBound = false
		return nil, fmt.Errorf("cannot peek repair headers, unexpected status %d", resp.StatusCode)
	}

	headers = rsp.Headers
	if headers["brand-id"] != brandID || headers["repair-id"] != strconv.Itoa(repairID) {
		return nil, fmt.Errorf("cannot peek repair headers, repair id mismatch %s/%s != %s/%d", headers["brand-id"], headers["repair-id"], brandID, repairID)
	}

	return headers, nil
}

// deviceInfo captures information about the device.
type deviceInfo struct {
	Brand string `json:"brand"`
	Model string `json:"model"`
}

// RepairStatus represents the possible statuses of a repair.
type RepairStatus int

const (
	RetryStatus RepairStatus = iota
	SkipStatus
	DoneStatus
)

func (rs RepairStatus) String() string {
	switch rs {
	case RetryStatus:
		return "retry"
	case SkipStatus:
		return "skip"
	case DoneStatus:
		return "done"
	default:
		return "unknown"
	}
}

// RepairState holds the current revision and status of a repair in a sequence of repairs.
type RepairState struct {
	Sequence int          `json:"sequence"`
	Revision int          `json:"revision"`
	Status   RepairStatus `json:"status"`
}

// state holds the atomically updated control state of the runner with sequences of repairs and their states.
type state struct {
	Device         deviceInfo                `json:"device"`
	Sequences      map[string][]*RepairState `json:"sequences,omitempty"`
	TimeLowerBound time.Time                 `json:"time-lower-bound"`
}

func (run *Runner) setRepairState(brandID string, state RepairState) {
	if run.state.Sequences == nil {
		run.state.Sequences = make(map[string][]*RepairState)
	}
	sequence := run.state.Sequences[brandID]
	if state.Sequence > len(sequence) {
		run.stateModified = true
		run.state.Sequences[brandID] = append(sequence, &state)
	} else if *sequence[state.Sequence-1] != state {
		run.stateModified = true
		sequence[state.Sequence-1] = &state
	}
}

func (run *Runner) readState() error {
	r, err := os.Open(dirs.SnapRepairStateFile)
	if err != nil {
		return err
	}
	defer r.Close()
	dec := json.NewDecoder(r)
	return dec.Decode(&run.state)
}

func (run *Runner) moveTimeLowerBound(t time.Time) {
	if t.After(run.state.TimeLowerBound) {
		run.stateModified = true
		run.state.TimeLowerBound = t.UTC()
	}
}

var timeNow = time.Now

func (run *Runner) now() time.Time {
	now := timeNow().UTC()
	if now.Before(run.state.TimeLowerBound) {
		return run.state.TimeLowerBound
	}
	return now
}

func (run *Runner) initState() error {
	if err := os.MkdirAll(dirs.SnapRepairDir, 0775); err != nil {
		return fmt.Errorf("cannot create repair state directory: %v", err)
	}
	// best-effort remove old
	os.Remove(dirs.SnapRepairStateFile)
	run.state = state{}
	// initialize time lower bound with image built time/seed.yaml time
	info, err := os.Stat(filepath.Join(dirs.SnapSeedDir, "seed.yaml"))
	if err != nil {
		return err
	}
	run.moveTimeLowerBound(info.ModTime())
	// initialize device info
	if err := run.initDeviceInfo(); err != nil {
		return err
	}
	run.stateModified = true
	return run.SaveState()
}

func trustedBackstore(trusted []asserts.Assertion) asserts.Backstore {
	trustedBS := asserts.NewMemoryBackstore()
	for _, t := range trusted {
		trustedBS.Put(t.Type(), t)
	}
	return trustedBS
}

func checkAuthorityID(a asserts.Assertion, trusted asserts.Backstore) error {
	assertType := a.Type()
	if assertType != asserts.AccountKeyType && assertType != asserts.AccountType {
		return nil
	}
	// check that account and account-key assertions are signed by
	// a trusted authority
	acctID := a.AuthorityID()
	_, err := trusted.Get(asserts.AccountType, []string{acctID}, asserts.AccountType.MaxSupportedFormat())
	if err != nil && !asserts.IsNotFound(err) {
		return err
	}
	if asserts.IsNotFound(err) {
		return fmt.Errorf("%v not signed by trusted authority: %s", a.Ref(), acctID)
	}
	return nil
}

func verifySignatures(a asserts.Assertion, workBS asserts.Backstore, trusted asserts.Backstore) error {
	if err := checkAuthorityID(a, trusted); err != nil {
		return err
	}
	acctKeyMaxSuppFormat := asserts.AccountKeyType.MaxSupportedFormat()

	seen := make(map[string]bool)
	bottom := false
	for !bottom {
		u := a.Ref().Unique()
		if seen[u] {
			return fmt.Errorf("circular assertions")
		}
		seen[u] = true
		signKey := []string{a.SignKeyID()}
		key, err := trusted.Get(asserts.AccountKeyType, signKey, acctKeyMaxSuppFormat)
		if err != nil && !asserts.IsNotFound(err) {
			return err
		}
		if err == nil {
			bottom = true
		} else {
			key, err = workBS.Get(asserts.AccountKeyType, signKey, acctKeyMaxSuppFormat)
			if err != nil && !asserts.IsNotFound(err) {
				return err
			}
			if asserts.IsNotFound(err) {
				return fmt.Errorf("cannot find public key %q", signKey[0])
			}
			if err := checkAuthorityID(key, trusted); err != nil {
				return err
			}
		}
		if err := asserts.CheckSignature(a, key.(*asserts.AccountKey), nil, time.Time{}); err != nil {
			return err
		}
		a = key
	}
	return nil
}

func (run *Runner) initDeviceInfo() error {
	const errPrefix = "cannot set device information: "

	workBS := asserts.NewMemoryBackstore()
	assertSeedDir := filepath.Join(dirs.SnapSeedDir, "assertions")
	dc, err := ioutil.ReadDir(assertSeedDir)
	if err != nil {
		return err
	}
	var model *asserts.Model
	for _, fi := range dc {
		fn := filepath.Join(assertSeedDir, fi.Name())
		f, err := os.Open(fn)
		if err != nil {
			// best effort
			continue
		}
		dec := asserts.NewDecoder(f)
		for {
			a, err := dec.Decode()
			if err != nil {
				// best effort
				break
			}
			switch a.Type() {
			case asserts.ModelType:
				if model != nil {
					return fmt.Errorf(errPrefix + "multiple models in seed assertions")
				}
				model = a.(*asserts.Model)
			case asserts.AccountType, asserts.AccountKeyType:
				workBS.Put(a.Type(), a)
			}
		}
	}
	if model == nil {
		return fmt.Errorf(errPrefix + "no model assertion in seed data")
	}
	trustedBS := trustedBackstore(sysdb.Trusted())
	if err := verifySignatures(model, workBS, trustedBS); err != nil {
		return fmt.Errorf(errPrefix+"%v", err)
	}
	acctPK := []string{model.BrandID()}
	acctMaxSupFormat := asserts.AccountType.MaxSupportedFormat()
	acct, err := trustedBS.Get(asserts.AccountType, acctPK, acctMaxSupFormat)
	if err != nil {
		var err error
		acct, err = workBS.Get(asserts.AccountType, acctPK, acctMaxSupFormat)
		if err != nil {
			return fmt.Errorf(errPrefix + "no brand account assertion in seed data")
		}
	}
	if err := verifySignatures(acct, workBS, trustedBS); err != nil {
		return fmt.Errorf(errPrefix+"%v", err)
	}
	run.state.Device.Brand = model.BrandID()
	run.state.Device.Model = model.Model()
	return nil
}

// LoadState loads the repairs' state from disk, and (re)initializes it if it's missing or corrupted.
func (run *Runner) LoadState() error {
	err := run.readState()
	if err == nil {
		return nil
	}
	// error => initialize from scratch
	if !os.IsNotExist(err) {
		logger.Noticef("cannor read repair state: %v", err)
	}
	return run.initState()
}

// SaveState saves the repairs' state to disk.
func (run *Runner) SaveState() error {
	if !run.stateModified {
		return nil
	}
	m, err := json.Marshal(&run.state)
	if err != nil {
		return fmt.Errorf("cannot marshal repair state: %v", err)
	}
	err = osutil.AtomicWriteFile(dirs.SnapRepairStateFile, m, 0600, 0)
	if err != nil {
		return fmt.Errorf("cannot save repair state: %v", err)
	}
	run.stateModified = false
	return nil
}

func stringList(headers map[string]interface{}, name string) ([]string, error) {
	v, ok := headers[name]
	if !ok {
		return nil, nil
	}
	l, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("header %q is not a list", name)
	}
	r := make([]string, len(l))
	for i, v := range l {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("header %q contains non-string elements", name)
		}
		r[i] = s
	}
	return r, nil
}

// Applicable returns whether a repair with the given headers is applicable to the device.
func (run *Runner) Applicable(headers map[string]interface{}) bool {
	series, err := stringList(headers, "series")
	if err != nil {
		return false
	}
	if len(series) != 0 && !strutil.ListContains(series, release.Series) {
		return false
	}
	archs, err := stringList(headers, "architectures")
	if err != nil {
		return false
	}
	if len(archs) != 0 && !strutil.ListContains(archs, arch.UbuntuArchitecture()) {
		return false
	}
	brandModel := fmt.Sprintf("%s/%s", run.state.Device.Brand, run.state.Device.Model)
	models, err := stringList(headers, "models")
	if err != nil {
		return false
	}
	if len(models) != 0 && !strutil.ListContains(models, brandModel) {
		// model prefix matching: brand/prefix*
		hit := false
		for _, patt := range models {
			if strings.HasSuffix(patt, "*") && strings.ContainsRune(patt, '/') {
				if strings.HasPrefix(brandModel, strings.TrimSuffix(patt, "*")) {
					hit = true
					break
				}
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

var errSkip = errors.New("repair unnecessary on this system")

func (run *Runner) fetch(brandID string, repairID int) (repair *asserts.Repair, aux []asserts.Assertion, err error) {
	headers, err := run.Peek(brandID, repairID)
	if err != nil {
		return nil, nil, err
	}
	if !run.Applicable(headers) {
		return nil, nil, errSkip
	}
	return run.Fetch(brandID, repairID, -1)
}

func (run *Runner) refetch(brandID string, repairID, revision int) (repair *asserts.Repair, aux []asserts.Assertion, err error) {
	return run.Fetch(brandID, repairID, revision)
}

func (run *Runner) saveStream(brandID string, repairID int, repair *asserts.Repair, aux []asserts.Assertion) error {
	d := filepath.Join(dirs.SnapRepairAssertsDir, brandID, strconv.Itoa(repairID))
	err := os.MkdirAll(d, 0775)
	if err != nil {
		return err
	}
	buf := &bytes.Buffer{}
	enc := asserts.NewEncoder(buf)
	r := append([]asserts.Assertion{repair}, aux...)
	for _, a := range r {
		if err := enc.Encode(a); err != nil {
			return fmt.Errorf("cannot encode repair assertions %s-%d for saving: %v", brandID, repairID, err)
		}
	}
	p := filepath.Join(d, fmt.Sprintf("r%d.repair", r[0].Revision()))
	return osutil.AtomicWriteFile(p, buf.Bytes(), 0600, 0)
}

func (run *Runner) readSavedStream(brandID string, repairID, revision int) (repair *asserts.Repair, aux []asserts.Assertion, err error) {
	d := filepath.Join(dirs.SnapRepairAssertsDir, brandID, strconv.Itoa(repairID))
	p := filepath.Join(d, fmt.Sprintf("r%d.repair", revision))
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	dec := asserts.NewDecoder(f)
	var r []asserts.Assertion
	for {
		a, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("cannot decode repair assertions %s-%d from disk: %v", brandID, repairID, err)
		}
		r = append(r, a)
	}
	return checkStream(brandID, repairID, r)
}

func (run *Runner) makeReady(brandID string, sequenceNext int) (repair *asserts.Repair, err error) {
	sequence := run.state.Sequences[brandID]
	var aux []asserts.Assertion
	var state RepairState
	if sequenceNext <= len(sequence) {
		// consider retries
		state = *sequence[sequenceNext-1]
		if state.Status != RetryStatus {
			return nil, errSkip
		}
		var err error
		repair, aux, err = run.refetch(brandID, state.Sequence, state.Revision)
		if err != nil {
			if err != ErrRepairNotModified {
				logger.Noticef("cannot refetch repair %s-%d, will retry what is on disk: %v", brandID, sequenceNext, err)
			}
			// try to use what we have already on disk
			repair, aux, err = run.readSavedStream(brandID, state.Sequence, state.Revision)
			if err != nil {
				return nil, err
			}
		}
	} else {
		// fetch the next repair in the sequence
		// assumes no gaps, each repair id is present so far,
		// possibly skipped
		var err error
		repair, aux, err = run.fetch(brandID, sequenceNext)
		if err != nil && err != errSkip {
			return nil, err
		}
		state = RepairState{
			Sequence: sequenceNext,
		}
		if err == errSkip {
			// TODO: store headers to justify decision
			state.Status = SkipStatus
			run.setRepairState(brandID, state)
			return nil, errSkip
		}
	}
	// verify with signatures
	if err := run.Verify(repair, aux); err != nil {
		return nil, fmt.Errorf("cannot verify repair %s-%d: %v", brandID, state.Sequence, err)
	}
	if err := run.saveStream(brandID, state.Sequence, repair, aux); err != nil {
		return nil, err
	}
	state.Revision = repair.Revision()
	if !run.Applicable(repair.Headers()) {
		state.Status = SkipStatus
		run.setRepairState(brandID, state)
		return nil, errSkip
	}
	run.setRepairState(brandID, state)
	return repair, nil
}

// Next returns the next repair for the brand id sequence to run/retry or ErrRepairNotFound if there is none atm. It updates the state as required.
func (run *Runner) Next(brandID string) (*Repair, error) {
	sequenceNext := run.sequenceNext[brandID]
	if sequenceNext == 0 {
		sequenceNext = 1
	}
	for {
		repair, err := run.makeReady(brandID, sequenceNext)
		// SaveState is a no-op unless makeReady modified the state
		stateErr := run.SaveState()
		if err != nil && err != errSkip && err != ErrRepairNotFound {
			// err is a non trivial error, just log the SaveState error and report err
			if stateErr != nil {
				logger.Noticef("%v", stateErr)
			}
			return nil, err
		}
		if stateErr != nil {
			return nil, stateErr
		}
		if err == ErrRepairNotFound {
			return nil, ErrRepairNotFound
		}

		sequenceNext += 1
		run.sequenceNext[brandID] = sequenceNext
		if err == errSkip {
			continue
		}

		return &Repair{
			Repair:   repair,
			run:      run,
			sequence: sequenceNext - 1,
		}, nil
	}
}

// Limit trust to specific keys while there's no delegation or limited
// keys support.  The obtained assertion stream may also include
// account keys that are directly or indirectly signed by a trusted
// key.
var (
	trustedRepairRootKeys []*asserts.AccountKey
)

// Verify verifies that the repair is properly signed by the specific
// trusted root keys or by account keys in the stream (passed via aux)
// directly or indirectly signed by a trusted key.
func (run *Runner) Verify(repair *asserts.Repair, aux []asserts.Assertion) error {
	workBS := asserts.NewMemoryBackstore()
	for _, a := range aux {
		if a.Type() != asserts.AccountKeyType {
			continue
		}
		err := workBS.Put(asserts.AccountKeyType, a)
		if err != nil {
			return err
		}
	}
	trustedBS := asserts.NewMemoryBackstore()
	for _, t := range trustedRepairRootKeys {
		trustedBS.Put(asserts.AccountKeyType, t)
	}
	for _, t := range sysdb.Trusted() {
		if t.Type() == asserts.AccountType {
			trustedBS.Put(asserts.AccountType, t)
		}
	}

	return verifySignatures(repair, workBS, trustedBS)
}