package renter

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/modules/renter"
	"github.com/NebulousLabs/Sia/node"
	"github.com/NebulousLabs/Sia/node/api"
	"github.com/NebulousLabs/Sia/node/api/client"
	"github.com/NebulousLabs/Sia/siatest"
	"github.com/NebulousLabs/Sia/types"

	"github.com/NebulousLabs/errors"
	"github.com/NebulousLabs/fastrand"
)

// TestRenter executes a number of subtests using the same TestGroup to
// save time on initialization
func TestRenter(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	// Create a group for the subtests
	groupParams := siatest.GroupParams{
		Hosts:   5,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Specify subtests to run
	subTests := []struct {
		name string
		test func(*testing.T, *siatest.TestGroup)
	}{
		{"TestRenterStreamingCache", testRenterStreamingCache},
		{"TestUploadDownload", testUploadDownload},
		{"TestSingleFileGet", testSingleFileGet},
		{"TestDownloadMultipleLargeSectors", testDownloadMultipleLargeSectors},
		{"TestRenterDownloadAfterRenew", testRenterDownloadAfterRenew},
		{"TestRenterLocalRepair", testRenterLocalRepair},
		{"TestRenterRemoteRepair", testRenterRemoteRepair},
		{"TestClearDownloadHistory", testClearDownloadHistory},
	}
	// Run subtests
	for _, subtest := range subTests {
		t.Run(subtest.name, func(t *testing.T) {
			subtest.test(t, tg)
		})
	}
}

// testUploadDownload is a subtest that uses an existing TestGroup to test if
// uploading and downloading a file works
func testUploadDownload(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	renter := tg.Renters()[0]
	// Upload file, creating a piece for each host in the group
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces
	fileSize := 100 + siatest.Fuzz()
	localFile, remoteFile, err := renter.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
	if err != nil {
		t.Fatal("Failed to upload a file for testing: ", err)
	}
	// Download the file synchronously directly into memory
	_, err = renter.DownloadByStream(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	// Download the file synchronously to a file on disk
	_, err = renter.DownloadToDisk(remoteFile, false)
	if err != nil {
		t.Fatal(err)
	}
	// Download the file asynchronously and wait for the download to finish.
	localFile, err = renter.DownloadToDisk(remoteFile, true)
	if err != nil {
		t.Error(err)
	}
	if err := renter.WaitForDownload(localFile, remoteFile); err != nil {
		t.Error(err)
	}
	// Stream the file.
	_, err = renter.Stream(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	// Stream the file partially a few times. At least 1 byte is streamed.
	for i := 0; i < 5; i++ {
		from := fastrand.Intn(fileSize - 1)             // [0..fileSize-2]
		to := from + 1 + fastrand.Intn(fileSize-from-1) // [from+1..fileSize-1]
		_, err = renter.StreamPartial(remoteFile, localFile, uint64(from), uint64(to))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// testSingleFileGet is a subtest that uses an existing TestGroup to test if
// using the single file API endpoint works
func testSingleFileGet(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	renter := tg.Renters()[0]
	// Upload file, creating a piece for each host in the group
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces
	fileSize := 100 + siatest.Fuzz()
	_, _, err := renter.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
	if err != nil {
		t.Fatal("Failed to upload a file for testing: ", err)
	}

	files, err := renter.Files()
	if err != nil {
		t.Fatal("Failed to get renter files: ", err)
	}

	var file modules.FileInfo
	for _, f := range files {
		file, err = renter.File(f.SiaPath)
		if err != nil {
			t.Fatal("Failed to request single file", err)
		}
		if file != f {
			t.Fatal("Single file queries does not match file previously requested.")
		}
	}
}

// testDownloadMultipleLargeSectors downloads multiple large files (>5 Sectors)
// in parallel and makes sure that the downloads are blocking each other.
func testDownloadMultipleLargeSectors(t *testing.T, tg *siatest.TestGroup) {
	// parallelDownloads is the number of downloads that are run in parallel.
	parallelDownloads := 10
	// fileSize is the size of the downloaded file.
	fileSize := int(10*modules.SectorSize) + siatest.Fuzz()
	// set download limits and reset them after test.
	// uniqueRemoteFiles is the number of files that will be uploaded to the
	// network. Downloads will choose the remote file to download randomly.
	uniqueRemoteFiles := 5
	// Grab the first of the group's renters
	renter := tg.Renters()[0]
	// set download limits and reset them after test.
	if err := renter.RenterPostRateLimit(int64(fileSize)*2, 0); err != nil {
		t.Fatal("failed to set renter bandwidth limit", err)
	}
	defer func() {
		if err := renter.RenterPostRateLimit(0, 0); err != nil {
			t.Error("failed to reset renter bandwidth limit", err)
		}
	}()

	// Upload files
	dataPieces := uint64(len(tg.Hosts())) - 1
	parityPieces := uint64(1)
	remoteFiles := make([]*siatest.RemoteFile, 0, uniqueRemoteFiles)
	for i := 0; i < uniqueRemoteFiles; i++ {
		_, remoteFile, err := renter.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
		if err != nil {
			t.Fatal("Failed to upload a file for testing: ", err)
		}
		remoteFiles = append(remoteFiles, remoteFile)
	}

	// Randomly download using download to file and download to stream methods.
	wg := new(sync.WaitGroup)
	for i := 0; i < parallelDownloads; i++ {
		wg.Add(1)
		go func() {
			var err error
			var rf = remoteFiles[fastrand.Intn(len(remoteFiles))]
			if fastrand.Intn(2) == 0 {
				_, err = renter.DownloadByStream(rf)
			} else {
				_, err = renter.DownloadToDisk(rf, false)
			}
			if err != nil {
				t.Error("Download failed:", err)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

// testRenterLocalRepair tests if a renter correctly repairs a file from disk
// after a host goes offline.
func testRenterLocalRepair(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	renter := tg.Renters()[0]

	// Check that we have enough hosts for this test.
	if len(tg.Hosts()) < 2 {
		t.Fatal("This test requires at least 2 hosts")
	}

	// Set fileSize and redundancy for upload
	fileSize := int(modules.SectorSize)
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces

	// Upload file
	_, remoteFile, err := renter.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}
	// Get the file info of the fully uploaded file. Tha way we can compare the
	// redundancies later.
	fi, err := renter.FileInfo(remoteFile)
	if err != nil {
		t.Fatal("failed to get file info", err)
	}

	// Take down one of the hosts and check if redundancy decreases.
	if err := tg.RemoveNode(tg.Hosts()[0]); err != nil {
		t.Fatal("Failed to shutdown host", err)
	}
	expectedRedundancy := float64(dataPieces+parityPieces-1) / float64(dataPieces)
	if err := renter.WaitForDecreasingRedundancy(remoteFile, expectedRedundancy); err != nil {
		t.Fatal("Redundancy isn't decreasing", err)
	}
	// We should still be able to download
	if _, err := renter.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download file", err)
	}
	// Bring up a new host and check if redundancy increments again.
	if err := tg.AddNodes(node.HostTemplate); err != nil {
		t.Fatal("Failed to create a new host", err)
	}
	if err := renter.WaitForUploadRedundancy(remoteFile, fi.Redundancy); err != nil {
		t.Fatal("File wasn't repaired", err)
	}
	// We should be able to download
	if _, err := renter.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download file", err)
	}
}

// testRenterRemoteRepair tests if a renter correctly repairs a file by
// downloading it after a host goes offline.
func testRenterRemoteRepair(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	r := tg.Renters()[0]

	// Check that we have enough hosts for this test.
	if len(tg.Hosts()) < 2 {
		t.Fatal("This test requires at least 2 hosts")
	}

	// Set fileSize and redundancy for upload
	fileSize := int(modules.SectorSize)
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces

	// Upload file
	localFile, remoteFile, err := r.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}
	// Get the file info of the fully uploaded file. Tha way we can compare the
	// redundancieslater.
	fi, err := r.FileInfo(remoteFile)
	if err != nil {
		t.Fatal("failed to get file info", err)
	}

	// Delete the file locally.
	if err := localFile.Delete(); err != nil {
		t.Fatal("failed to delete local file", err)
	}

	// Take down all of the parity hosts and check if redundancy decreases.
	for i := uint64(0); i < parityPieces; i++ {
		if err := tg.RemoveNode(tg.Hosts()[0]); err != nil {
			t.Fatal("Failed to shutdown host", err)
		}
	}
	expectedRedundancy := float64(dataPieces+parityPieces-1) / float64(dataPieces)
	if err := r.WaitForDecreasingRedundancy(remoteFile, expectedRedundancy); err != nil {
		t.Fatal("Redundancy isn't decreasing", err)
	}
	// We should still be able to download
	if _, err := r.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download file", err)
	}
	// Bring up new parity hosts and check if redundancy increments again.
	if err := tg.AddNodeN(node.HostTemplate, int(parityPieces)); err != nil {
		t.Fatal("Failed to create a new host", err)
	}
	// When doing remote repair the redundancy might not reach 100%.
	expectedRedundancy = (1.0 - renter.RemoteRepairDownloadThreshold) * fi.Redundancy
	if err := r.WaitForUploadRedundancy(remoteFile, expectedRedundancy); err != nil {
		t.Fatal("File wasn't repaired", err)
	}
	// We should be able to download
	if _, err := r.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download file", err)
	}
}

// The following four tests can not be run in parallel as it causes a panic
// of `too many files open

// TestDownloadInterruptedBeforeSendingRevision runs testDownloadInterrupted
// with a dependency that interrupts the download before sending the signed
// revision to the host.
func TestDownloadInterruptedBeforeSendingRevision(t *testing.T) {
	testDownloadInterrupted(t, newDependencyInterruptDownloadBeforeSendingRevision())
}

// TestDownloadInterruptedAfterSendingRevision runs testDownloadInterrupted
// with a dependency that interrupts the download after sending the signed
// revision to the host.
func TestDownloadInterruptedAfterSendingRevision(t *testing.T) {
	testDownloadInterrupted(t, newDependencyInterruptDownloadAfterSendingRevision())
}

// TestUploadInterruptedBeforeSendingRevision runs testUploadInterrupted with a
// dependency that interrupts the upload before sending the signed revision to
// the host.
func TestUploadInterruptedBeforeSendingRevision(t *testing.T) {
	testUploadInterrupted(t, newDependencyInterruptUploadBeforeSendingRevision())
}

// TestUploadInterruptedAfterSendingRevision runs testUploadInterrupted with a
// dependency that interrupts the upload after sending the signed revision to
// the host.
func TestUploadInterruptedAfterSendingRevision(t *testing.T) {
	testUploadInterrupted(t, newDependencyInterruptUploadAfterSendingRevision())
}

// testDownloadInterrupted interrupts a download using the provided dependencies.
func testDownloadInterrupted(t *testing.T, deps *siatest.DependencyInterruptOnceOnKeyword) {
	if testing.Short() {
		t.SkipNow()
	}

	// Get a directory for testing.
	testDir, err := siatest.TestDir(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	// Create a group with a single renter and five hosts using the dependencies
	// for the renter.
	renterTemplate := node.Renter(testDir + "/renter")
	renterTemplate.ContractSetDeps = deps
	tg, err := siatest.NewGroup(renterTemplate, siatest.Miner(testDir+"/miner"))
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a few hosts to the group.
	if err := tg.AddNodeN(node.HostTemplate, 5); err != nil {
		t.Fatal(err)
	}

	// Upload a file that's 1 chunk large.
	renter := tg.Renters()[0]
	dataPieces := uint64(len(tg.Hosts())) - 1
	parityPieces := uint64(1)
	chunkSize := siatest.ChunkSize(uint64(dataPieces))
	_, remoteFile, err := renter.UploadNewFileBlocking(int(chunkSize), dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}

	// Set the bandwidth limit to 1 chunk per second.
	if err := renter.RenterPostRateLimit(int64(chunkSize), int64(chunkSize)); err != nil {
		t.Fatal(err)
	}

	// Call fail on the dependency every 100 ms.
	cancel := make(chan struct{})
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		for {
			// Cause the next download to fail.
			deps.Fail()
			select {
			case <-cancel:
				wg.Done()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	// Try downloading the file 5 times.
	for i := 0; i < 5; i++ {
		if _, err := renter.DownloadByStream(remoteFile); err == nil {
			t.Fatal("Download shouldn't succeed since it was interrupted")
		}
	}
	// Stop calling fail on the dependency.
	close(cancel)
	wg.Wait()
	deps.Disable()
	// Download the file once more successfully
	if _, err := renter.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download the file", err)
	}
}

// testUploadInterrupted let's the upload fail using the provided dependencies
// and makes sure that this doesn't corrupt the contract.
func testUploadInterrupted(t *testing.T, deps *siatest.DependencyInterruptOnceOnKeyword) {
	if testing.Short() {
		t.SkipNow()
	}

	// Get a directory for testing.
	testDir, err := siatest.TestDir(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	// Create a group with a single renter and five hosts using the dependencies
	// for the renter.
	renterTemplate := node.Renter(testDir + "/renter")
	renterTemplate.ContractSetDeps = deps
	tg, err := siatest.NewGroup(renterTemplate, siatest.Miner(testDir+"/miner"))
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a few hosts to the group.
	if err := tg.AddNodeN(node.HostTemplate, 5); err != nil {
		t.Fatal(err)
	}

	// Set the bandwidth limit to 1 chunk per second.
	renter := tg.Renters()[0]
	dataPieces := uint64(len(tg.Hosts())) - 1
	parityPieces := uint64(1)
	chunkSize := siatest.ChunkSize(uint64(dataPieces))
	if err := renter.RenterPostRateLimit(int64(chunkSize), int64(chunkSize)); err != nil {
		t.Fatal(err)
	}

	// Call fail on the dependency every two seconds to allow some uploads to
	// finish.
	cancel := make(chan struct{})
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		// Loop until cancel was closed or we reach 5 iterations. Otherwise we
		// might end up blocking the upload for too long.
		for i := 0; i < 5; i++ {
			// Cause the next upload to fail.
			deps.Fail()
			select {
			case <-cancel:
				wg.Done()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		wg.Done()
	}()

	// Upload a file that's 1 chunk large.
	_, remoteFile, err := renter.UploadNewFileBlocking(int(chunkSize), dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}
	// Stop calling fail on the dependency.
	close(cancel)
	wg.Wait()
	deps.Disable()
	// Download the file.
	if _, err := renter.DownloadByStream(remoteFile); err != nil {
		t.Fatal("Failed to download the file", err)
	}
}

// testRenterStreamingCache checks if the chunk cache works correctly.
func testRenterStreamingCache(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	r := tg.Renters()[0]

	// Testing setting StreamCacheSize for streaming
	// Test setting it to larger than the defaultCacheSize
	if err := r.RenterSetStreamCacheSizePost(4); err != nil {
		t.Fatal(err, "Could not set StreamCacheSize to 4")
	}
	rg, err := r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize != 4 {
		t.Fatal("StreamCacheSize not set to 4, set to", rg.Settings.StreamCacheSize)
	}

	// Test resetting to the value of defaultStreamCacheSize (2)
	if err := r.RenterSetStreamCacheSizePost(2); err != nil {
		t.Fatal(err, "Could not set StreamCacheSize to 2")
	}
	rg, err = r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize != 2 {
		t.Fatal("StreamCacheSize not set to 2, set to", rg.Settings.StreamCacheSize)
	}

	prev := rg.Settings.StreamCacheSize

	// Test setting to 0
	if err := r.RenterSetStreamCacheSizePost(0); err == nil {
		t.Fatal(err, "expected setting stream cache size to zero to fail with an error")
	}
	rg, err = r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize == 0 {
		t.Fatal("StreamCacheSize set to 0, should have stayed as previous value or", prev)
	}

	// Set fileSize and redundancy for upload
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces

	// Set the bandwidth limit to 1 chunk per second.
	pieceSize := modules.SectorSize - crypto.TwofishOverhead
	chunkSize := int64(pieceSize * dataPieces)
	if err := r.RenterPostRateLimit(chunkSize, chunkSize); err != nil {
		t.Fatal(err)
	}

	rg, err = r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not request RenterGe()")
	}
	if rg.Settings.MaxDownloadSpeed != chunkSize {
		t.Fatal(errors.New("MaxDownloadSpeed doesn't match value set through RenterPostRateLimit"))
	}
	if rg.Settings.MaxUploadSpeed != chunkSize {
		t.Fatal(errors.New("MaxUploadSpeed doesn't match value set through RenterPostRateLimit"))
	}

	// Upload a file that is a single chunk big.
	_, remoteFile, err := r.UploadNewFileBlocking(int(chunkSize), dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}

	// Download the same chunk 250 times. This should take at least 250 seconds
	// without caching but not more than 30 with caching.
	start := time.Now()
	for i := 0; i < 250; i++ {
		if _, err := r.Stream(remoteFile); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > time.Second*30 {
			t.Fatal("download took longer than 30 seconds")
		}
	}
}

// TestRenewFailing checks if a contract gets marked as !goodForRenew after
// failing multiple times in a row.
func TestRenewFailing(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()
	renterDir, err := siatest.TestDir(filepath.Join(t.Name(), "renter"))
	if err != nil {
		t.Fatal(err)
	}

	// Create a group for the subtests
	groupParams := siatest.GroupParams{
		Hosts:  3,
		Miners: 1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a renter with a custom allowance to give it plenty of time to renew
	// the contract later.
	renterParams := node.Renter(renterDir)
	renterParams.Allowance = siatest.DefaultAllowance
	renterParams.Allowance.Hosts = uint64(len(tg.Hosts()) - 1)
	renterParams.Allowance.Period = 100
	renterParams.Allowance.RenewWindow = 50
	if err = tg.AddNodes(renterParams); err != nil {
		t.Fatal(err)
	}
	renter := tg.Renters()[0]

	// All the contracts of the renter should be goodForRenew.
	rcg, err := renter.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rcg.Contracts {
		if !c.GoodForRenew {
			t.Fatal("renter got a contract that is !goodForRenew")
		}
	}
	if uint64(len(rcg.Contracts)) != renterParams.Allowance.Hosts {
		for i, c := range rcg.Contracts {
			fmt.Println(i, c.HostPublicKey)
		}
		t.Fatalf("renter had %v contracts but should have %v",
			len(rcg.Contracts), renterParams.Allowance.Hosts)
	}

	// Create a map of the hosts in the group.
	hostMap := make(map[string]*siatest.TestNode)
	for _, host := range tg.Hosts() {
		pk, err := host.HostPublicKey()
		if err != nil {
			t.Fatal(err)
		}
		hostMap[pk.String()] = host
	}
	// Lock the wallet of one of the used hosts to make the renew fail.
	for _, c := range rcg.Contracts {
		if host, used := hostMap[c.HostPublicKey.String()]; used {
			if err := host.WalletLockPost(); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	// Wait until the contract is supposed to be renewed.
	cg, err := renter.ConsensusGet()
	if err != nil {
		t.Fatal(err)
	}
	rg, err := renter.RenterGet()
	if err != nil {
		t.Fatal(err)
	}
	miner := tg.Miners()[0]
	blockHeight := cg.Height
	for blockHeight+rg.Settings.Allowance.RenewWindow < rcg.Contracts[0].EndHeight {
		if err := miner.MineBlock(); err != nil {
			t.Fatal(err)
		}
		blockHeight++
	}

	// contracts should still be goodForRenew.
	rcg, err = renter.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rcg.Contracts {
		if !c.GoodForRenew {
			t.Fatal("renter got a contract that is !goodForRenew")
		}
	}

	// mine enough blocks to reach the second half of the renew window.
	for ; blockHeight+rg.Settings.Allowance.RenewWindow/2 < rcg.Contracts[0].EndHeight; blockHeight++ {
		if err := miner.MineBlock(); err != nil {
			t.Fatal(err)
		}
	}

	// We should be within the second half of the renew window now. We keep
	// mining blocks until the host with the locked wallet has been replaced.
	// This should happen before we reach the endHeight of the contracts.
	replaced := false
	err = build.Retry(int(rcg.Contracts[0].EndHeight-blockHeight), 5*time.Second, func() error {
		// contract should be !goodForRenew now.
		rcg, err = renter.RenterActiveContractsGet()
		if err != nil {
			t.Fatal(err)
		}
		notGoodForRenew := 0
		goodForRenew := 0
		for _, c := range rcg.Contracts {
			if !c.GoodForRenew {
				notGoodForRenew++
			} else {
				goodForRenew++
			}
		}
		if err := miner.MineBlock(); err != nil {
			return err
		}
		if !replaced && notGoodForRenew != 1 && goodForRenew != 1 {
			return fmt.Errorf("there should be exactly 1 contract that is !goodForRenew but was %v",
				notGoodForRenew)
		}
		replaced = true
		if replaced && notGoodForRenew != 1 && goodForRenew != 2 {
			return fmt.Errorf("contract was set to !goodForRenew but hasn't been replaced yet")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenterPersistData checks if the RenterSettings are persisted
func TestRenterPersistData(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Get test directory
	testdir, err := siatest.TestDir(t.Name())
	if err != nil {
		t.Fatal(err)
	}

	// Copying legacy file to test directory
	renterDir := filepath.Join(testdir, "renter")
	destination := filepath.Join(renterDir, "renter.json")
	err = os.MkdirAll(renterDir, 0700)
	if err != nil {
		t.Fatal(err)
	}
	from, err := os.Open("../../compatibility/renter_v04.json")
	if err != nil {
		t.Fatal(err)
	}
	to, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(to, from)
	if err != nil {
		t.Fatal(err)
	}
	if err = from.Close(); err != nil {
		t.Fatal(err)
	}
	if err = to.Close(); err != nil {
		t.Fatal(err)
	}

	// Create new node from legacy renter.json persistence file
	r, err := siatest.NewNode(node.AllModules(testdir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err = r.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Set renter allowance to finish renter set up
	// Currently /renter POST endpoint errors if the allowance
	// is not previously set or passed in as an argument
	err = r.RenterPostAllowance(siatest.DefaultAllowance)
	if err != nil {
		t.Fatal(err)
	}

	// Check Settings, should be defaults
	rg, err := r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize != renter.DefaultStreamCacheSize {
		t.Fatalf("StreamCacheSize not set to default of %v, set to %v",
			renter.DefaultStreamCacheSize, rg.Settings.StreamCacheSize)
	}
	if rg.Settings.MaxDownloadSpeed != renter.DefaultMaxDownloadSpeed {
		t.Fatalf("MaxDownloadSpeed not set to default of %v, set to %v",
			renter.DefaultMaxDownloadSpeed, rg.Settings.MaxDownloadSpeed)
	}
	if rg.Settings.MaxUploadSpeed != renter.DefaultMaxUploadSpeed {
		t.Fatalf("MaxUploadSpeed not set to default of %v, set to %v",
			renter.DefaultMaxUploadSpeed, rg.Settings.MaxUploadSpeed)
	}

	// Set StreamCacheSize, MaxDownloadSpeed, and MaxUploadSpeed to new values
	cacheSize := uint64(4)
	ds := int64(20)
	us := int64(10)
	if err := r.RenterSetStreamCacheSizePost(cacheSize); err != nil {
		t.Fatalf("%v: Could not set StreamCacheSize to %v", err, cacheSize)
	}
	if err := r.RenterPostRateLimit(ds, us); err != nil {
		t.Fatalf("%v: Could not set RateLimits to %v and %v", err, ds, us)
	}

	// Confirm Settings were updated
	rg, err = r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize != cacheSize {
		t.Fatalf("StreamCacheSize not set to %v, set to %v", cacheSize, rg.Settings.StreamCacheSize)
	}
	if rg.Settings.MaxDownloadSpeed != ds {
		t.Fatalf("MaxDownloadSpeed not set to %v, set to %v", ds, rg.Settings.MaxDownloadSpeed)
	}
	if rg.Settings.MaxUploadSpeed != us {
		t.Fatalf("MaxUploadSpeed not set to %v, set to %v", us, rg.Settings.MaxUploadSpeed)
	}

	// Restart node
	err = r.RestartNode()
	if err != nil {
		t.Fatal("Failed to restart node:", err)
	}

	// check Settings, settings should be values set through API endpoints
	rg, err = r.RenterGet()
	if err != nil {
		t.Fatal(err, "Could not get Renter through RenterGet()")
	}
	if rg.Settings.StreamCacheSize != cacheSize {
		t.Fatalf("StreamCacheSize not persisted as %v, set to %v", cacheSize, rg.Settings.StreamCacheSize)
	}
	if rg.Settings.MaxDownloadSpeed != ds {
		t.Fatalf("MaxDownloadSpeed not persisted as %v, set to %v", ds, rg.Settings.MaxDownloadSpeed)
	}
	if rg.Settings.MaxUploadSpeed != us {
		t.Fatalf("MaxUploadSpeed not persisted as %v, set to %v", us, rg.Settings.MaxUploadSpeed)
	}
}

// testRenterDownloadAfterRenew makes sure that we can still download a file
// after the contract period has ended.
func testRenterDownloadAfterRenew(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	renter := tg.Renters()[0]
	// Upload file, creating a piece for each host in the group
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts())) - dataPieces
	fileSize := 100 + siatest.Fuzz()
	_, remoteFile, err := renter.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
	if err != nil {
		t.Fatal("Failed to upload a file for testing: ", err)
	}
	// Mine enough blocks for the next period to start. This means the
	// contracts should be renewed and the data should still be available for
	// download.
	miner := tg.Miners()[0]
	for i := types.BlockHeight(0); i < siatest.DefaultAllowance.Period; i++ {
		if err := miner.MineBlock(); err != nil {
			t.Fatal(err)
		}
	}
	// Download the file synchronously directly into memory.
	_, err = renter.DownloadByStream(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenterContractEndHeight makes sure that the endheight of renewed
// contracts is set properly
func TestRenterContractEndHeight(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a group for the subtests
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Get Renter
	r := tg.Renters()[0]
	rg, err := r.RenterGet()
	if err != nil {
		t.Fatal("Could not get renter:", err)
	}

	// Record the start period at the beginning of test
	currentPeriodStart := rg.CurrentPeriod
	period := rg.Settings.Allowance.Period
	renewWindow := rg.Settings.Allowance.RenewWindow
	numRenewals := 0

	// Confirm Contracts were created as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	rcActive, err := r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err, "could not get active contracts")
	}

	// Confirm contract end heights were set properly
	for _, c := range rcActive.Contracts {
		if c.EndHeight != currentPeriodStart+period {
			t.Log("Endheight:", c.EndHeight)
			t.Log("Allowance Period:", period)
			t.Log("Current Period:", currentPeriodStart)
			t.Fatal("Contract endheight not set to Current period + Allowance Period")
		}
	}

	// Mine blocks to force contract renewal
	if err = renewContractsByRenewWindow(r, tg); err != nil {
		t.Fatal(err)
	}
	numRenewals++

	// Confirm Contracts were renewed as expected, all original contracts should
	// have been renewed if GoodForRenew = true
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		if err = checkRenewedContracts(rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Confirm contract end heights were set properly End height should be the
	// end of the next period as the contracts are renewed due to reaching the
	// renew window
	rcActive, err = r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal("Could not get renter contracts:", err)
	}
	for _, c := range rcActive.Contracts {
		if c.EndHeight != currentPeriodStart+(2*period)-renewWindow && c.GoodForRenew {
			t.Log("Endheight:", c.EndHeight)
			t.Log("Allowance Period:", period)
			t.Log("Renew Window:", renewWindow)
			t.Log("Current Period:", currentPeriodStart)
			t.Fatal("Contract endheight not set to Current period + 2 * Allowance Period - Renew Window")
		}
	}

	// Capturing end height to compare against renewed contracts
	endHeight := rcActive.Contracts[0].EndHeight

	// Renew contracts by running out of funds
	startingUploadSpend, err := renewContractsBySpending(r, tg)
	if err != nil {
		t.Fatal(err)
	}

	// Confirm contract end heights were set properly
	// End height should not have changed since the renewal
	// was due to running out of funds
	rcActive, err = r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal("Could not get renter contracts:", err)
	}
	for _, c := range rcActive.Contracts {
		if c.EndHeight != endHeight && c.GoodForRenew && c.UploadSpending.Cmp(startingUploadSpend) <= 0 {
			t.Log("Allowance Period:", period)
			t.Log("Current Period:", currentPeriodStart)
			t.Fatalf("Contract endheight Changed, EH was %v, expected %v\n", c.EndHeight, endHeight)
		}
	}
}

// TestRenterSpendingReporting checks the accuracy for the reported
// spending
func TestRenterSpendingReporting(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a testgroup, creating without renter so the renter's
	// initial balance can be obtained
	groupParams := siatest.GroupParams{
		Hosts:  2,
		Miners: 1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Add a Renter node
	renterDir, err := siatest.TestDir(filepath.Join(t.Name(), "renter"))
	if err != nil {
		t.Fatal(err)
	}
	renterParams := node.Renter(renterDir)
	renterParams.SkipSetAllowance = true
	if err = tg.AddNodes(renterParams); err != nil {
		t.Fatal(err)
	}

	// Get largest WindowSize from Hosts
	var windowSize types.BlockHeight
	for _, h := range tg.Hosts() {
		hg, err := h.HostGet()
		if err != nil {
			t.Fatal(err)
		}
		if hg.ExternalSettings.WindowSize >= windowSize {
			windowSize = hg.ExternalSettings.WindowSize
		}
	}

	// Get renter's initial siacoin balance
	r := tg.Renters()[0]
	wg, err := r.WalletGet()
	if err != nil {
		t.Fatal("Failed to get wallet:", err)
	}
	initialBalance := wg.ConfirmedSiacoinBalance

	// Set allowance
	if err = tg.SetRenterAllowance(r, siatest.DefaultAllowance); err != nil {
		t.Fatal("Failed to set renter allowance:", err)
	}
	numRenewals := 0

	// Confirm Contracts were created as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check that the funds allocated when setting the allowance
	// are reflected correctly in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Upload and download files to show spending
	var remoteFiles []*siatest.RemoteFile
	for i := 0; i < 10; i++ {
		dataPieces := uint64(1)
		parityPieces := uint64(1)
		fileSize := 100 + siatest.Fuzz()
		_, rf, err := r.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
		if err != nil {
			t.Fatal("Failed to upload a file for testing: ", err)
		}
		remoteFiles = append(remoteFiles, rf)
	}
	for _, rf := range remoteFiles {
		_, err = r.DownloadToDisk(rf, false)
		if err != nil {
			t.Fatal("Could not DownloadToDisk:", err)
		}
	}

	// Check to confirm upload and download spending was captured correctly
	// and reflected in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine blocks to force contract renewal
	if err = renewContractsByRenewWindow(r, tg); err != nil {
		t.Fatal(err)
	}
	numRenewals++

	// Confirm Contracts were renewed as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		if err = checkRenewedContracts(rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine Block to confirm contracts and spending into blockchain
	m := tg.Miners()[0]
	if err = m.MineBlock(); err != nil {
		t.Fatal("Error mining block:", err)
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Check contract spending against reported spending
	rcActive, err := r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcInactive, err := r.RenterInactiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcExpired, err := r.RenterExpiredContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if err = checkContractVsReportedSpending(r, windowSize, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
		t.Fatal(err)
	}

	// Check to confirm reported spending is still accurate with the renewed contracts
	// and reflected in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record current Wallet Balance
	wg, err = r.WalletGet()
	if err != nil {
		t.Fatal("Failed to get wallet:", err)
	}
	initialPeriodEndBalance := wg.ConfirmedSiacoinBalance

	// Mine blocks to force contract renewal and new period
	cg, _ := r.ConsensusGet()
	blockHeight := cg.Height
	endHeight := rcActive.Contracts[0].EndHeight
	rg, err := r.RenterGet()
	if err != nil {
		t.Fatal("Failed to get renter:", err)
	}
	rw := rg.Settings.Allowance.RenewWindow
	for i := 0; i < int(endHeight-rw-blockHeight+types.MaturityDelay); i++ {
		if err = m.MineBlock(); err != nil {
			t.Fatal("Error mining block:", err)
		}
	}
	numRenewals++

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Check if Unspent unallocated funds were released after allowance period
	// was exceeded
	wg, err = r.WalletGet()
	if err != nil {
		t.Fatal("Failed to get wallet:", err)
	}
	if initialPeriodEndBalance.Cmp(wg.ConfirmedSiacoinBalance) > 0 {
		t.Fatal("Unspent Unallocated funds not released after contract renewal and maturity delay")
	}

	// Confirm Contracts were renewed as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		if err = checkRenewedContracts(rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine Block to confirm contracts and spending on blockchain
	if err = m.MineBlock(); err != nil {
		t.Fatal("Error mining block:", err)
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Check contract spending against reported spending
	rcActive, err = r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcInactive, err = r.RenterInactiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcExpired, err = r.RenterExpiredContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if err = checkContractVsReportedSpending(r, windowSize, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
		t.Fatal(err)
	}

	// Check to confirm reported spending is still accurate with the renewed contracts
	// and a new period and reflected in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Renew contracts by running out of funds
	_, err = renewContractsBySpending(r, tg)
	if err != nil {
		t.Fatal(err)
	}
	numRenewals++

	// Mine Block to confirm contracts and spending on blockchain
	if err = m.MineBlock(); err != nil {
		t.Fatal("Error mining block:", err)
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Confirm Contracts were renewed as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		if err = checkRenewedContracts(rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine Block to confirm contracts and spending on blockchain
	if err = m.MineBlock(); err != nil {
		t.Fatal("Error mining block:", err)
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Check contract spending against reported spending
	rcActive, err = r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcInactive, err = r.RenterInactiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcExpired, err = r.RenterExpiredContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if err = checkContractVsReportedSpending(r, windowSize, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
		t.Fatal(err)
	}

	// Check to confirm reported spending is still accurate with the renewed contracts
	// and a new period and reflected in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine blocks to force contract renewal
	if err = renewContractsByRenewWindow(r, tg); err != nil {
		t.Fatal(err)
	}
	numRenewals++

	// Confirm Contracts were renewed as expected
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err := r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get Inactive contracts")
		}
		rcExpired, err := r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if err = checkContracts(len(tg.Hosts()), numRenewals, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
			return err
		}
		if err = checkRenewedContracts(rcActive.Contracts); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mine Block to confirm contracts and spending into blockchain
	if err = m.MineBlock(); err != nil {
		t.Fatal("Error mining block:", err)
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Check contract spending against reported spending
	rcActive, err = r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcInactive, err = r.RenterInactiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcExpired, err = r.RenterExpiredContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if err = checkContractVsReportedSpending(r, windowSize, append(rcInactive.Contracts, rcExpired.Contracts...), rcActive.Contracts); err != nil {
		t.Fatal(err)
	}

	// Check to confirm reported spending is still accurate with the renewed contracts
	// and reflected in the wallet balance
	err = build.Retry(600, 100*time.Millisecond, func() error {
		err = checkBalanceVsSpending(r, initialBalance)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkBalanceVsSpending checks the renters confirmed siacoin balance in their
// wallet against their reported spending
func checkBalanceVsSpending(r *siatest.TestNode, initialBalance types.Currency) error {
	// Getting initial financial metrics
	// Setting variables to easier reference
	rg, err := r.RenterGet()
	if err != nil {
		return err
	}
	fm := rg.FinancialMetrics

	// Check balance after allowance is set
	wg, err := r.WalletGet()
	if err != nil {
		return err
	}
	expectedBalance := initialBalance.Sub(fm.TotalAllocated).Sub(fm.WithheldFunds).Sub(fm.PreviousSpending)
	if expectedBalance.Cmp(wg.ConfirmedSiacoinBalance) != 0 {
		details := fmt.Sprintf(`Initial balance minus Renter Reported Spending does not equal wallet Confirmed Siacoin Balance
		Expected Balance:   %v
		Wallet Balance:     %v
		Actual difference:  %v
		ExpectedBalance:    %v
		walletBalance:      %v
		`, expectedBalance.HumanString(), wg.ConfirmedSiacoinBalance.HumanString(), initialBalance.Sub(wg.ConfirmedSiacoinBalance).HumanString(),
			expectedBalance.HumanString(), wg.ConfirmedSiacoinBalance.HumanString())
		var diff string
		if expectedBalance.Cmp(wg.ConfirmedSiacoinBalance) > 0 {
			diff = fmt.Sprintf("Under reported by:  %v\n", expectedBalance.Sub(wg.ConfirmedSiacoinBalance).HumanString())
		} else {
			diff = fmt.Sprintf("Over reported by:   %v\n", wg.ConfirmedSiacoinBalance.Sub(expectedBalance).HumanString())
		}
		err := details + diff
		return errors.New(err)
	}
	return nil
}

// checkContracts confirms that contracts are renewed as expected, renewed
// contracts should be the renter's active contracts and oldContracts should be
// the renter's inactive and expired contracts
func checkContracts(numHosts, numRenewals int, oldContracts, renewedContracts []api.RenterContract) error {
	if len(renewedContracts) != numHosts {
		err := fmt.Sprintf("Incorrect number of Active contracts: have %v expected %v", len(renewedContracts), numHosts)
		return errors.New(err)
	}
	if len(oldContracts) == 0 && numRenewals == 0 {
		return nil
	}
	// Confirm contracts were renewed, this will also mean there are old contracts
	// Verify there are not more renewedContracts than there are oldContracts
	// This would mean contracts are not getting archived
	if len(oldContracts) < len(renewedContracts) {
		return errors.New("Too many renewed contracts")
	}
	if len(oldContracts) != numHosts*numRenewals {
		err := fmt.Sprintf("Incorrect number of Old contracts: have %v expected %v", len(oldContracts), numHosts*numRenewals)
		return errors.New(err)
	}

	// Create Maps for comparison
	initialContractIDMap := make(map[types.FileContractID]struct{})
	initialContractKeyMap := make(map[crypto.Hash]struct{})
	for _, c := range oldContracts {
		initialContractIDMap[c.ID] = struct{}{}
		initialContractKeyMap[crypto.HashBytes(c.HostPublicKey.Key)] = struct{}{}
	}

	for _, c := range renewedContracts {
		// Verify that all the contracts marked as GoodForRenew
		// were renewed
		if _, ok := initialContractIDMap[c.ID]; ok {
			return errors.New("ID from renewedContracts found in oldContracts")
		}
		// Verifying that Renewed Contracts have the same HostPublicKey
		// as an initial contract
		if _, ok := initialContractKeyMap[crypto.HashBytes(c.HostPublicKey.Key)]; !ok {
			return errors.New("Host Public Key from renewedContracts not found in oldContracts")
		}
	}
	return nil
}

// checkRenewedContracts confirms that renewed contracts have zero upload and
// download spending. Renewed contracts should be the renter's active contracts
func checkRenewedContracts(renewedContracts []api.RenterContract) error {
	for _, c := range renewedContracts {
		if c.UploadSpending.Cmp(types.ZeroCurrency) != 0 && c.GoodForUpload {
			err := fmt.Sprintf("Upload spending on renewed contract equal to %v, expected zero", c.UploadSpending.HumanString())
			return errors.New(err)
		}
		if c.DownloadSpending.Cmp(types.ZeroCurrency) != 0 {
			err := fmt.Sprintf("Download spending on renewed contract equal to %v, expected zero", c.DownloadSpending.HumanString())
			return errors.New(err)
		}
	}
	return nil
}

// checkContractVsReportedSpending confirms that the spending recorded in the
// renter's contracts matches the reported spending for the renter. Renewed
// contracts should be the renter's active contracts and oldContracts should be
// the renter's inactive and expired contracts
func checkContractVsReportedSpending(r *siatest.TestNode, WindowSize types.BlockHeight, oldContracts, renewedContracts []api.RenterContract) error {
	// Get Current BlockHeight
	cg, err := r.ConsensusGet()
	if err != nil {
		return err
	}

	// Getting financial metrics after uploads, downloads, and
	// contract renewal
	rg, err := r.RenterGet()
	if err != nil {
		return err
	}

	fm := rg.FinancialMetrics
	totalSpent := fm.ContractFees.Add(fm.UploadSpending).
		Add(fm.DownloadSpending).Add(fm.StorageSpending)
	total := totalSpent.Add(fm.Unspent)
	allowance := rg.Settings.Allowance

	// Check that renter financial metrics add up to allowance
	if total.Cmp(allowance.Funds) != 0 {
		err := fmt.Sprintf(`Combined Total of reported spending and unspent funds not equal to allowance:
			total:     %v
			allowance: %v
			`, total.HumanString(), allowance.Funds.HumanString())
		return errors.New(err)
	}

	// Check renter financial metrics against contract spending
	var spending modules.ContractorSpending
	for _, contract := range oldContracts {
		if contract.StartHeight >= rg.CurrentPeriod {
			// Calculate ContractFees
			spending.ContractFees = spending.ContractFees.Add(contract.Fees)
			// Calculate TotalAllocated
			spending.TotalAllocated = spending.TotalAllocated.Add(contract.TotalCost)
			// Calculate Spending
			spending.DownloadSpending = spending.DownloadSpending.Add(contract.DownloadSpending)
			spending.UploadSpending = spending.UploadSpending.Add(contract.UploadSpending)
			spending.StorageSpending = spending.StorageSpending.Add(contract.StorageSpending)
		} else if contract.EndHeight+WindowSize+types.MaturityDelay > cg.Height {
			// Calculated funds that are being withheld in contracts
			spending.WithheldFunds = spending.WithheldFunds.Add(contract.RenterFunds)
			// Record the largest window size for worst case when reporting the spending
			if contract.EndHeight+WindowSize+types.MaturityDelay >= spending.ReleaseBlock {
				spending.ReleaseBlock = contract.EndHeight + WindowSize + types.MaturityDelay
			}
			// Calculate Previous spending
			spending.PreviousSpending = spending.PreviousSpending.Add(contract.Fees).
				Add(contract.DownloadSpending).Add(contract.UploadSpending).Add(contract.StorageSpending)
		} else {
			// Calculate Previous spending
			spending.PreviousSpending = spending.PreviousSpending.Add(contract.Fees).
				Add(contract.DownloadSpending).Add(contract.UploadSpending).Add(contract.StorageSpending)
		}
	}
	for _, contract := range renewedContracts {
		if contract.GoodForRenew {
			// Calculate ContractFees
			spending.ContractFees = spending.ContractFees.Add(contract.Fees)
			// Calculate TotalAllocated
			spending.TotalAllocated = spending.TotalAllocated.Add(contract.TotalCost)
			// Calculate Spending
			spending.DownloadSpending = spending.DownloadSpending.Add(contract.DownloadSpending)
			spending.UploadSpending = spending.UploadSpending.Add(contract.UploadSpending)
			spending.StorageSpending = spending.StorageSpending.Add(contract.StorageSpending)
		}
	}

	// Compare contract fees
	if fm.ContractFees.Cmp(spending.ContractFees) != 0 {
		err := fmt.Sprintf(`Fees not equal:
			Financial Metrics Fees: %v
			Contract Fees:          %v
			`, fm.ContractFees.HumanString(), spending.ContractFees.HumanString())
		return errors.New(err)
	}
	// Compare Total Allocated
	if fm.TotalAllocated.Cmp(spending.TotalAllocated) != 0 {
		err := fmt.Sprintf(`Total Allocated not equal:
			Financial Metrics TA: %v
			Contract TA:          %v
			`, fm.TotalAllocated.HumanString(), spending.TotalAllocated.HumanString())
		return errors.New(err)
	}
	// Compare Upload Spending
	if fm.UploadSpending.Cmp(spending.UploadSpending) != 0 {
		err := fmt.Sprintf(`Upload spending not equal:
			Financial Metrics US: %v
			Contract US:          %v
			`, fm.UploadSpending.HumanString(), spending.UploadSpending.HumanString())
		return errors.New(err)
	}
	// Compare Download Spending
	if fm.DownloadSpending.Cmp(spending.DownloadSpending) != 0 {
		err := fmt.Sprintf(`Download spending not equal:
			Financial Metrics DS: %v
			Contract DS:          %v
			`, fm.DownloadSpending.HumanString(), spending.DownloadSpending.HumanString())
		return errors.New(err)
	}
	// Compare Storage Spending
	if fm.StorageSpending.Cmp(spending.StorageSpending) != 0 {
		err := fmt.Sprintf(`Storage spending not equal:
			Financial Metrics SS: %v
			Contract SS:          %v
			`, fm.StorageSpending.HumanString(), spending.StorageSpending.HumanString())
		return errors.New(err)
	}
	// Compare Withheld Funds
	if fm.WithheldFunds.Cmp(spending.WithheldFunds) != 0 {
		err := fmt.Sprintf(`Withheld Funds not equal:
			Financial Metrics WF: %v
			Contract WF:          %v
			`, fm.WithheldFunds.HumanString(), spending.WithheldFunds.HumanString())
		return errors.New(err)
	}
	// Compare Release Block
	if fm.ReleaseBlock != spending.ReleaseBlock {
		err := fmt.Sprintf(`Release Block not equal:
			Financial Metrics RB: %v
			Contract RB:          %v
			`, fm.ReleaseBlock, spending.ReleaseBlock)
		return errors.New(err)
	}
	// Compare Previous Spending
	if fm.PreviousSpending.Cmp(spending.PreviousSpending) != 0 {
		err := fmt.Sprintf(`Previous spending not equal:
			Financial Metrics PS: %v
			Contract PS:          %v
			`, fm.PreviousSpending.HumanString(), spending.PreviousSpending.HumanString())
		return errors.New(err)
	}

	return nil
}

// renewContractByRenewWindow mines blocks to force contract renewal
//
// TODO:
// 1) remove excess tg.Sync() calls in the test since it is called in this
// function
// 2) make this more generic by referencing the contracts
func renewContractsByRenewWindow(renter *siatest.TestNode, tg *siatest.TestGroup) error {
	rg, err := renter.RenterGet()
	if err != nil {
		return errors.AddContext(err, "failed to get RenterGet")
	}
	m := tg.Miners()[0]
	for i := 0; i < int(rg.Settings.Allowance.Period-rg.Settings.Allowance.RenewWindow); i++ {
		if err = m.MineBlock(); err != nil {
			return errors.AddContext(err, "error mining block")
		}
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		return err
	}
	return nil
}

// renewContractsBySpending uploads files until the contracts renew due to
// running out of funds
func renewContractsBySpending(renter *siatest.TestNode, tg *siatest.TestGroup) (startingUploadSpend types.Currency, err error) {
	// Renew contracts by running out of funds
	// Set upload price to max price
	maxStoragePrice := types.SiacoinPrecision.Mul64(30e3).Div(modules.BlockBytesPerMonthTerabyte) // 30k SC / TB / Month
	maxUploadPrice := maxStoragePrice.Mul64(3 * 4320)
	hosts := tg.Hosts()
	for _, h := range hosts {
		err := h.HostModifySettingPost(client.HostParamMinUploadBandwidthPrice, maxUploadPrice)
		if err != nil {
			return types.ZeroCurrency, errors.AddContext(err, "could not set Host Upload Price")
		}
	}

	// Waiting for nodes to sync
	m := tg.Miners()[0]
	if err := m.MineBlock(); err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "error mining block")
	}
	if err := tg.Sync(); err != nil {
		return types.ZeroCurrency, err
	}

	// Set upload parameters
	dataPieces := uint64(1)
	parityPieces := uint64(1)
	chunkSize := siatest.ChunkSize(1)

	// Upload once to show upload spending
	_, _, err = renter.UploadNewFileBlocking(int(chunkSize), dataPieces, parityPieces)
	if err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "failed to upload a file for testing")
	}

	// Get current upload spend, previously contracts had zero upload spend
	rcActive, err := renter.RenterActiveContractsGet()
	if err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "could not get renter active contracts")
	}
	rcInactive, err := renter.RenterInactiveContractsGet()
	if err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "could not get renter inactive contracts")
	}
	rcExpired, err := renter.RenterExpiredContractsGet()
	if err != nil {
		return types.ZeroCurrency, errors.AddContext(err, "could not get renter expired contracts")
	}
	startingUploadSpend = rcActive.Contracts[0].UploadSpending
	numberExpiredContracts := len(rcInactive.Contracts) + len(rcExpired.Contracts)

	// Upload files to force contract renewal due to running out of funds
	//
	// TODO: Can the for loop condition be removed so only the internal break
	// check is used.  This would save time by eliminating two API calls
LOOP:
	for len(rcInactive.Contracts)+len(rcExpired.Contracts) == numberExpiredContracts {
		// To protect against contracts not renewing during uploads
		for _, c := range rcActive.Contracts {
			percentRemaining, _ := big.NewRat(0, 1).SetFrac(c.RenterFunds.Big(), c.TotalCost.Big()).Float64()
			if percentRemaining < float64(0.03) {
				break LOOP
			}
		}
		_, _, err = renter.UploadNewFileBlocking(int(chunkSize), dataPieces, parityPieces)
		if err != nil {
			return types.ZeroCurrency, errors.AddContext(err, "failed to upload a file for testing")
		}

		rcActive, err = renter.RenterActiveContractsGet()
		if err != nil {
			return types.ZeroCurrency, errors.AddContext(err, "could not get renter active contracts")
		}
		rcInactive, err = renter.RenterInactiveContractsGet()
		if err != nil {
			return types.ZeroCurrency, errors.AddContext(err, "could not get renter inactive contracts")
		}
		rcExpired, err = renter.RenterExpiredContractsGet()
		if err != nil {
			return types.ZeroCurrency, errors.AddContext(err, "could not get renter expired contracts")
		}
	}
	return startingUploadSpend, nil
}

// TestRedundancyReporting verifies that redundancy reporting is accurate if
// contracts become offline.
func TestRedundancyReporting(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a group for testing.
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Upload a file.
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts()) - 1)

	renter := tg.Renters()[0]
	_, rf, err := renter.UploadNewFileBlocking(100, dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}

	// Stop a host.
	host := tg.Hosts()[0]
	if err := tg.StopNode(host); err != nil {
		t.Fatal(err)
	}

	// Mine a block to trigger contract maintenance.
	miner := tg.Miners()[0]
	if err := miner.MineBlock(); err != nil {
		t.Fatal(err)
	}

	// Redundancy should decrease.
	expectedRedundancy := float64(dataPieces+parityPieces-1) / float64(dataPieces)
	if err := renter.WaitForDecreasingRedundancy(rf, expectedRedundancy); err != nil {
		t.Fatal("Redundancy isn't decreasing", err)
	}

	// Restart the host.
	if err := tg.StartNode(host); err != nil {
		t.Fatal(err)
	}

	// Wait until the host shows up as active again.
	pk, err := host.HostPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	err = build.Retry(60, time.Second, func() error {
		hdag, err := renter.HostDbActiveGet()
		if err != nil {
			return err
		}
		for _, h := range hdag.Hosts {
			if reflect.DeepEqual(h.PublicKey, pk) {
				return nil
			}
		}
		// If host is not active, announce it again and mine a block.
		if err := host.HostAnnouncePost(); err != nil {
			return (err)
		}
		miner := tg.Miners()[0]
		if err := miner.MineBlock(); err != nil {
			return (err)
		}
		if err := tg.Sync(); err != nil {
			return (err)
		}
		hg, err := host.HostGet()
		if err != nil {
			return err
		}
		return fmt.Errorf("host with address %v not active", hg.InternalSettings.NetAddress)
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := miner.MineBlock(); err != nil {
		t.Fatal(err)
	}

	// Redundancy should go back to normal.
	expectedRedundancy = float64(dataPieces+parityPieces) / float64(dataPieces)
	if err := renter.WaitForUploadRedundancy(rf, expectedRedundancy); err != nil {
		t.Fatal("Redundancy is not increasing")
	}
}

// TestRenterCancelAllowance tests that setting an empty allowance causes
// uploads, downloads, and renewals to cease.
func TestRenterCancelAllowance(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a group for testing.
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Upload a file.
	dataPieces := uint64(1)
	parityPieces := uint64(len(tg.Hosts()) - 1)

	renter := tg.Renters()[0]
	_, rf, err := renter.UploadNewFileBlocking(100, dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the allowance
	if err := renter.RenterCancelAllowance(); err != nil {
		t.Fatal(err)
	}

	// Give it some time to mark the contracts as !goodForUpload and
	// !goodForRenew.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := renter.RenterActiveContractsGet()
		if err != nil {
			return err
		}
		rcInactive, err := renter.RenterInactiveContractsGet()
		if err != nil {
			return err
		}
		// Should now have 2 inactive contracts.
		if len(rcActive.Contracts) != 0 {
			return fmt.Errorf("expected 0 active contracts, got %v", len(rcActive.Contracts))
		}
		if len(rcInactive.Contracts) != groupParams.Hosts {
			return fmt.Errorf("expected %v inactive contracts, got %v", groupParams.Hosts, len(rcInactive.Contracts))
		}
		for _, c := range rcInactive.Contracts {
			if c.GoodForUpload {
				return errors.New("contract shouldn't be goodForUpload")
			}
			if c.GoodForRenew {
				return errors.New("contract shouldn't be goodForRenew")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Try downloading the file; should succeed.
	if _, err := renter.DownloadByStream(rf); err != nil {
		t.Fatal("downloading file failed", err)
	}

	// Wait for a few seconds to make sure that the upload heap is rebuilt.
	// The rebuilt interval is 3 seconds. Sleep for 5 to be safe.
	time.Sleep(5 * time.Second)

	// Try to upload a file after the allowance was cancelled. Should fail.
	_, rf2, err := renter.UploadNewFile(100, dataPieces, parityPieces)
	if err != nil {
		t.Fatal(err)
	}

	// Give it some time to upload.
	time.Sleep(time.Second)

	// Redundancy should still be 0.
	renterFiles, err := renter.RenterFilesGet()
	if err != nil {
		t.Fatal("Failed to get files")
	}
	if len(renterFiles.Files) != 2 {
		t.Fatal("There should be exactly 2 tracked files")
	}
	fileInfo, err := renter.File(rf2.SiaPath())
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.UploadProgress > 0 || fileInfo.UploadedBytes > 0 || fileInfo.Redundancy > 0 {
		t.Fatal("Uploading a file after canceling the allowance should fail")
	}

	// Mine enough blocks for the period to pass and the contracts to expire.
	miner := tg.Miners()[0]
	for i := types.BlockHeight(0); i < siatest.DefaultAllowance.Period; i++ {
		if err := miner.MineBlock(); err != nil {
			t.Fatal(err)
		}
	}

	// All contracts should be archived.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := renter.RenterActiveContractsGet()
		if err != nil {
			return err
		}
		rcInactive, err := renter.RenterInactiveContractsGet()
		if err != nil {
			return err
		}
		rcExpired, err := renter.RenterExpiredContractsGet()
		if err != nil {
			return err
		}
		// Should now have 2 expired contracts.
		if len(rcActive.Contracts) != 0 {
			return fmt.Errorf("expected 0 active contracts, got %v", len(rcActive.Contracts))
		}
		if len(rcInactive.Contracts) != 0 {
			return fmt.Errorf("expected 0 inactive contracts, got %v", len(rcInactive.Contracts))
		}
		if len(rcExpired.Contracts) != groupParams.Hosts {
			return fmt.Errorf("expected %v expired contracts, got %v", groupParams.Hosts, len(rcInactive.Contracts))
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}

	// Try downloading the file; should fail.
	if _, err := renter.DownloadByStream(rf2); err == nil {
		t.Error("downloading file succeeded even though it shouldnt", err)
	}

	// The uploaded files should have 0x redundancy now.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rf, err := renter.RenterFilesGet()
		if err != nil {
			return errors.New("Failed to get files")
		}
		if len(rf.Files) != 2 || rf.Files[0].Redundancy != 0 || rf.Files[1].Redundancy != 0 {
			return errors.New("file redundancy should be 0 now")
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// TestRenterCancelAllowance tests that setting an empty allowance causes
// uploads, downloads, and renewals to cease.
func TestRenterResetAllowance(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a group for testing.
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	renter := tg.Renters()[0]

	// Cancel the allowance
	if err := renter.RenterCancelAllowance(); err != nil {
		t.Fatal(err)
	}

	// Give it some time to mark the contracts as !goodForUpload and
	// !goodForRenew.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := renter.RenterActiveContractsGet()
		if err != nil {
			return err
		}
		rcInactive, err := renter.RenterInactiveContractsGet()
		if err != nil {
			return err
		}
		// Should now have 2 inactive contracts.
		if len(rcActive.Contracts) != 0 {
			return fmt.Errorf("expected 0 active contracts, got %v", len(rcActive.Contracts))
		}
		if len(rcInactive.Contracts) != groupParams.Hosts {
			return fmt.Errorf("expected %v inactive contracts, got %v", groupParams.Hosts, len(rcInactive.Contracts))
		}
		for _, c := range rcInactive.Contracts {
			if c.GoodForUpload {
				return errors.New("contract shouldn't be goodForUpload")
			}
			if c.GoodForRenew {
				return errors.New("contract shouldn't be goodForRenew")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Set the allowance again.
	if err := renter.RenterPostAllowance(siatest.DefaultAllowance); err != nil {
		t.Fatal(err)
	}

	// Mine a block to start the threadedContractMaintenance.
	if err := tg.Miners()[0].MineBlock(); err != nil {
		t.Fatal(err)
	}

	// Give it some time to mark the contracts as goodForUpload and
	// goodForRenew again.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err := renter.RenterActiveContractsGet()
		if err != nil {
			return err
		}
		// Should now have 2 active contracts.
		if len(rcActive.Contracts) != groupParams.Hosts {
			return fmt.Errorf("expected %v active contracts, got %v", groupParams.Hosts, len(rcActive.Contracts))
		}
		for _, c := range rcActive.Contracts {
			if !c.GoodForUpload {
				return errors.New("contract should be goodForUpload")
			}
			if !c.GoodForRenew {
				return errors.New("contract should be goodForRenew")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenterContractsEndpoint tests the API endpoint for old contracts
func TestRenterContractsEndpoint(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Create a group for testing.
	groupParams := siatest.GroupParams{
		Hosts:   2,
		Renters: 1,
		Miners:  1,
	}
	tg, err := siatest.NewGroupFromTemplate(groupParams)
	if err != nil {
		t.Fatal("Failed to create group: ", err)
	}
	defer func() {
		if err := tg.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Get Renter
	r := tg.Renters()[0]

	// Renter should only have active contracts
	rcAll, err := r.RenterContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	rcActive, err := r.RenterActiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if len(rcAll.Contracts) != len(rcActive.Contracts) {
		t.Fatalf("Expected the same number for active and all contracts: %v for active and %v for all", len(rcActive.Contracts), len(rcAll.Contracts))
	}
	// Create Maps for comparison
	ContractIDMap := make(map[types.FileContractID]struct{})
	for _, c := range rcAll.Contracts {
		ContractIDMap[c.ID] = struct{}{}
	}
	// Verify rcActive and rcAll have the same contracts
	for _, c := range rcActive.Contracts {
		if _, ok := ContractIDMap[c.ID]; !ok {
			t.Fatal("ID from rcActive found in rcAll")
		}
	}
	rcInactive, err := r.RenterInactiveContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if len(rcInactive.Contracts) != 0 {
		t.Fatal("Expected zero inactive contracts, got", len(rcInactive.Contracts))
	}
	rcExpired, err := r.RenterExpiredContractsGet()
	if err != nil {
		t.Fatal(err)
	}
	if len(rcExpired.Contracts) != 0 {
		t.Fatal("Expected zero expired contracts, got", len(rcExpired.Contracts))
	}

	// Record original Contracts and create Maps for comparison
	originalContracts := rcActive.Contracts
	originalContractIDMap := make(map[types.FileContractID]struct{})
	for _, c := range originalContracts {
		originalContractIDMap[c.ID] = struct{}{}
	}

	// Renew contracts
	// Mine blocks to force contract renewal
	if err = renewContractsByRenewWindow(r, tg); err != nil {
		t.Fatal(err)
	}
	numRenewals := 1
	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Confirm contracts were renewed as expected, there should be no expired
	// contracts since we are still within the endheight of the original
	// contracts, there should be the same number of active and inactive
	// contracts, and the inactive contracts should be the same contracts as the
	// original active contracts.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		// Check active and expired contracts
		rcActive, err = r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err = r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get inactive contracts")
		}
		if len(rcActive.Contracts) != len(rcInactive.Contracts) {
			return errors.New(fmt.Sprintf("Expected the same number of active and inactive contracts; got %v active and %v inactive", len(rcActive.Contracts), len(rcInactive.Contracts)))
		}
		if len(originalContracts) != len(rcInactive.Contracts) {
			return errors.New(fmt.Sprintf("Didn't get expected number of inactive contracts, expected %v got %v", len(originalContracts), len(rcInactive.Contracts)))
		}
		for _, c := range rcInactive.Contracts {
			if _, ok := originalContractIDMap[c.ID]; !ok {
				return errors.New("ID from rcInactive not found in originalContracts")
			}
		}

		// Check expired contracts
		rcExpired, err = r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if len(rcExpired.Contracts) != 0 {
			return errors.New(fmt.Sprintf("Expected zero expired contracts, got %v", len(rcExpired.Contracts)))
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record inactive contracts
	// Record current active and expired contracts
	rcInactive, err = r.RenterInactiveContractsGet()
	inactiveContracts := rcInactive.Contracts
	if err != nil {
		t.Fatal(err)
	}
	inactiveContractIDMap := make(map[types.FileContractID]struct{})
	for _, c := range inactiveContracts {
		inactiveContractIDMap[c.ID] = struct{}{}
	}

	// Mine to force inactive contracts to be expired contracts
	m := tg.Miners()[0]
	rg, err := r.RenterGet()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < int(rg.Settings.Allowance.RenewWindow); i++ {
		if err = m.MineBlock(); err != nil {
			t.Fatal(err)
		}
	}

	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	// Confirm contracts, the expired contracts should now be the same contracts
	// as the previous inactive contracts.
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcExpired, err = r.RenterExpiredContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get expired contracts")
		}
		if len(rcExpired.Contracts) != len(inactiveContracts) {
			return errors.New(fmt.Sprintf("Expected the same number of expired and inactive contracts; got %v expired and %v inactive", len(rcExpired.Contracts), len(inactiveContracts)))
		}
		for _, c := range inactiveContracts {
			if _, ok := inactiveContractIDMap[c.ID]; !ok {
				return errors.New("ID from rcExpired not found in inactiveContracts")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record current active and expired contracts
	rcActive, err = r.RenterActiveContractsGet()
	activeContracts := rcActive.Contracts
	if err != nil {
		t.Fatal(err)
	}
	activeContractIDMap := make(map[types.FileContractID]struct{})
	for _, c := range activeContracts {
		activeContractIDMap[c.ID] = struct{}{}
	}
	rcExpired, err = r.RenterExpiredContractsGet()
	expiredContracts := rcExpired.Contracts
	if err != nil {
		t.Fatal(err)
	}
	expiredContractIDMap := make(map[types.FileContractID]struct{})
	for _, c := range expiredContracts {
		expiredContractIDMap[c.ID] = struct{}{}
	}

	//*****DEBUG*******
	cg, _ := r.ConsensusGet()
	fmt.Println("BH", cg.Height)
	rg, _ = r.RenterGet()
	fmt.Println("CP", rg.CurrentPeriod)
	fmt.Println("All")
	rcAll, _ = r.RenterContractsGet()
	for _, c := range rcAll.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Active")
	for _, c := range rcActive.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Inactive")
	rcInactive, _ = r.RenterInactiveContractsGet()
	for _, c := range rcInactive.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Expired")
	for _, c := range rcExpired.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	//**********************

	// Renew contracts by spending
	_, err = renewContractsBySpending(r, tg)
	if err != nil {
		t.Fatal(err)
	}
	numRenewals++
	// Waiting for nodes to sync
	if err = tg.Sync(); err != nil {
		t.Fatal(err)
	}

	//*****DEBUG*******
	cg, _ = r.ConsensusGet()
	fmt.Println("BH", cg.Height)
	rg, _ = r.RenterGet()
	fmt.Println("CP", rg.CurrentPeriod)
	fmt.Println("All")
	rcAll, _ = r.RenterContractsGet()
	for _, c := range rcAll.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Active")
	rcActive, _ = r.RenterActiveContractsGet()
	for _, c := range rcActive.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Inactive")
	rcInactive, _ = r.RenterInactiveContractsGet()
	for _, c := range rcInactive.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	fmt.Println("Expired")
	rcExpired, _ = r.RenterExpiredContractsGet()
	for _, c := range rcExpired.Contracts {
		fmt.Println("ID", c.ID)
		fmt.Println("SH", c.StartHeight)
		fmt.Println("EH", c.EndHeight)
		fmt.Println("GFR", c.GoodForRenew)
		fmt.Println("GFU", c.GoodForUpload)
	}
	//**********************

	// Confirm contracts were renewed as expected.  Active contracts prior to
	// renewal should now be in the inactive contracts
	err = build.Retry(600, 100*time.Millisecond, func() error {
		rcActive, err = r.RenterActiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get active contracts")
		}
		rcInactive, err = r.RenterInactiveContractsGet()
		if err != nil {
			return errors.AddContext(err, "could not get inactive contracts")
		}
		// rcExpired, err = r.RenterExpiredContractsGet()
		// if err != nil {
		// 	return errors.AddContext(err, "could not get expired contracts")
		// }

		// // Check for the same number of contracts
		// if len(rcActive.Contracts) != len(rcExpired.Contracts) || len(rcActive.Contracts) != len(rcInactive.Contracts) {
		// 	errStr := fmt.Sprintf(`
		// 		Expected the same number of contracts, instead got:
		// 		Active:   %v
		// 		Inactive: %v
		// 		Expired:  %v
		// 		`, len(rcActive.Contracts), len(rcInactive.Contracts), len(rcExpired.Contracts))
		// 	return errors.New(errStr)
		// }

		// Confirm active and inactive contracts
		// if len(activeContracts) != len(rcInactive.Contracts) {
		// 	return errors.New(fmt.Sprintf("Didn't get expected number of inactive contracts, expected %v got %v", len(activeContracts), len(rcInactive.Contracts)))
		// }
		inactiveContractIDMap := make(map[types.FileContractID]struct{})
		for _, c := range rcInactive.Contracts {
			inactiveContractIDMap[c.ID] = struct{}{}
		}
		for _, c := range activeContracts {
			if _, ok := inactiveContractIDMap[c.ID]; !ok {
				return errors.New("ID from activeContacts not found in rcInactive")
			}
		}

		// // Confirm expired contracts
		// if len(expiredContracts) != len(rcExpired.Contracts) {
		// 	return errors.New(fmt.Sprintf("Didn't get expected number of expired contracts, expected %v got %v", len(expiredContracts), len(rcExpired.Contracts)))
		// }
		// for _, c := range rcExpired.Contracts {
		// 	if _, ok := expiredContractIDMap[c.ID]; !ok {
		// 		return errors.New("ID from rcExpired not found in expiredContracts")
		// 	}
		// }

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// testClearDownloadHistory makes sure that the download history is
// properly cleared when called through the API
func testClearDownloadHistory(t *testing.T, tg *siatest.TestGroup) {
	// Grab the first of the group's renters
	r := tg.Renters()[0]

	rdg, err := r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	numDownloads := 10
	if len(rdg.Downloads) < numDownloads {
		remainingDownloads := numDownloads - len(rdg.Downloads)
		rf, err := r.RenterFilesGet()
		if err != nil {
			t.Fatal(err)
		}
		// Check if the renter has any files
		// Upload a file if none
		if len(rf.Files) == 0 {
			dataPieces := uint64(1)
			parityPieces := uint64(1)
			fileSize := 100 + siatest.Fuzz()
			_, _, err := r.UploadNewFileBlocking(fileSize, dataPieces, parityPieces)
			if err != nil {
				t.Fatal("Failed to upload a file for testing: ", err)
			}
			rf, err = r.RenterFilesGet()
			if err != nil {
				t.Fatal(err)
			}
		}
		// Download files to build download history
		dest := filepath.Join(siatest.SiaTestingDir, strconv.Itoa(fastrand.Intn(math.MaxInt32)))
		for i := 0; i < remainingDownloads; i++ {
			err = r.RenterDownloadGet(rf.Files[0].SiaPath, dest, 0, rf.Files[0].Filesize, false)
			if err != nil {
				t.Fatal("Could not Download file:", err)
			}
		}
		rdg, err = r.RenterDownloadsGet()
		if err != nil {
			t.Fatal("Could not get download history:", err)
		}
		// Confirm download history is not empty
		if len(rdg.Downloads) != numDownloads {
			t.Fatalf("Not all downloads added to download history: only %v downloads added, expected %v", len(rdg.Downloads), numDownloads)
		}
	}
	numDownloads = len(rdg.Downloads)

	// Check removing one download from history
	// Remove First Download
	timestamp := rdg.Downloads[0].StartTime
	err = r.RenterClearDownloadsRangePost(timestamp, timestamp)
	if err != nil {
		t.Fatal("Error in API endpoint to remove download from history:", err)
	}
	numDownloads--
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	if len(rdg.Downloads) != numDownloads {
		t.Fatalf("Download history not reduced: history has %v downloads, expected %v", len(rdg.Downloads), numDownloads)
	}
	i := sort.Search(len(rdg.Downloads), func(i int) bool { return rdg.Downloads[i].StartTime.Equal(timestamp) })
	if i < len(rdg.Downloads) {
		t.Fatal("Specified download not removed from history")
	}
	// Remove Last Download
	timestamp = rdg.Downloads[len(rdg.Downloads)-1].StartTime
	err = r.RenterClearDownloadsRangePost(timestamp, timestamp)
	if err != nil {
		t.Fatal("Error in API endpoint to remove download from history:", err)
	}
	numDownloads--
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	if len(rdg.Downloads) != numDownloads {
		t.Fatalf("Download history not reduced: history has %v downloads, expected %v", len(rdg.Downloads), numDownloads)
	}
	i = sort.Search(len(rdg.Downloads), func(i int) bool { return rdg.Downloads[i].StartTime.Equal(timestamp) })
	if i < len(rdg.Downloads) {
		t.Fatal("Specified download not removed from history")
	}

	// Check Clear Before
	timestamp = rdg.Downloads[len(rdg.Downloads)-2].StartTime
	err = r.RenterClearDownloadsBeforePost(timestamp)
	if err != nil {
		t.Fatal("Error in API endpoint to clear download history before timestamp:", err)
	}
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	i = sort.Search(len(rdg.Downloads), func(i int) bool { return rdg.Downloads[i].StartTime.Before(timestamp) })
	if i < len(rdg.Downloads) {
		t.Fatal("Download found that was before given time")
	}

	// Check Clear After
	timestamp = rdg.Downloads[1].StartTime
	err = r.RenterClearDownloadsAfterPost(timestamp)
	if err != nil {
		t.Fatal("Error in API endpoint to clear download history after timestamp:", err)
	}
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	i = sort.Search(len(rdg.Downloads), func(i int) bool { return rdg.Downloads[i].StartTime.After(timestamp) })
	if i < len(rdg.Downloads) {
		t.Fatal("Download found that was after given time")
	}

	// Check clear range
	before := rdg.Downloads[1].StartTime
	after := rdg.Downloads[len(rdg.Downloads)-1].StartTime
	err = r.RenterClearDownloadsRangePost(after, before)
	if err != nil {
		t.Fatal("Error in API endpoint to remove range of downloads from history:", err)
	}
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	i = sort.Search(len(rdg.Downloads), func(i int) bool {
		return rdg.Downloads[i].StartTime.Before(before) && rdg.Downloads[i].StartTime.After(after)
	})
	if i < len(rdg.Downloads) {
		t.Fatal("Not all downloads from range removed from history")
	}

	// Check clearing download history
	err = r.RenterClearAllDownloadsPost()
	if err != nil {
		t.Fatal("Error in API endpoint to clear download history:", err)
	}
	rdg, err = r.RenterDownloadsGet()
	if err != nil {
		t.Fatal("Could not get download history:", err)
	}
	if len(rdg.Downloads) != 0 {
		t.Fatalf("Download history not cleared: history has %v downloads, expected 0", len(rdg.Downloads))
	}
}
