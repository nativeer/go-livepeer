/*
Package server is the place we integrate the Livepeer node with the LPMS media server.
*/
package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/go-livepeer/drivers"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/net"

	"github.com/cenkalti/backoff"
	"github.com/ericxtang/m3u8"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	lpmscore "github.com/livepeer/lpms/core"
	ffmpeg "github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/segmenter"
	"github.com/livepeer/lpms/stream"
	"github.com/livepeer/lpms/vidplayer"
)

var ErrAlreadyExists = errors.New("StreamAlreadyExists")
var ErrRTMPPublish = errors.New("ErrRTMPPublish")
var ErrBroadcast = errors.New("ErrBroadcast")
var ErrHLSPlay = errors.New("ErrHLSPlay")
var ErrRTMPPlay = errors.New("ErrRTMPPlay")
var ErrRoundInit = errors.New("ErrRoundInit")
var ErrStorage = errors.New("ErrStorage")
var ErrDiscovery = errors.New("ErrDiscovery")
var ErrUnknownStream = errors.New("ErrUnknownStream")

const HLSWaitInterval = time.Second
const HLSBufferCap = uint(43200) //12 hrs assuming 1s segment
const HLSBufferWindow = uint(5)
const StreamKeyBytes = 6

const SegLen = 2 * time.Second
const HLSUnsubWorkerFreq = time.Second * 5
const BroadcastRetry = 15 * time.Second

var BroadcastPrice = big.NewInt(1)
var BroadcastJobVideoProfiles = []ffmpeg.VideoProfile{ffmpeg.P240p30fps4x3, ffmpeg.P360p30fps16x9}
var MinDepositSegmentCount = int64(75) // 5 mins assuming 4s segments
var LastHLSStreamID core.StreamID
var LastManifestID core.ManifestID

type rtmpConnection struct {
	nonce   uint64
	stream  stream.RTMPVideoStream
	pl      core.PlaylistManager
	sess    *BroadcastSession
	profile *ffmpeg.VideoProfile
}

type LivepeerServer struct {
	RTMPSegmenter lpmscore.RTMPSegmenter
	LPMS          *lpmscore.LPMS
	LivepeerNode  *core.LivepeerNode
	HttpMux       *http.ServeMux

	ExposeCurrentManifest bool

	rtmpConnections map[core.ManifestID]*rtmpConnection
	connectionLock  *sync.RWMutex
}

func NewLivepeerServer(rtmpAddr string, httpAddr string, lpNode *core.LivepeerNode) *LivepeerServer {
	opts := lpmscore.LPMSOpts{
		RtmpAddr: rtmpAddr, RtmpDisabled: true,
		HttpAddr: httpAddr,
		WorkDir:  lpNode.WorkDir,
	}
	switch lpNode.NodeType {
	case core.BroadcasterNode:
		opts.RtmpDisabled = false
	case core.OrchestratorNode:
		opts.HttpMux = http.NewServeMux()
	}
	server := lpmscore.New(&opts)
	return &LivepeerServer{RTMPSegmenter: server, LPMS: server, LivepeerNode: lpNode, HttpMux: opts.HttpMux, connectionLock: &sync.RWMutex{}, rtmpConnections: make(map[core.ManifestID]*rtmpConnection)}
}

//StartServer starts the LPMS server
func (s *LivepeerServer) StartMediaServer(ctx context.Context, maxPricePerSegment *big.Int, transcodingOptions string) error {
	if s.LivepeerNode.Eth != nil {
		BroadcastPrice = maxPricePerSegment
		bProfiles := make([]ffmpeg.VideoProfile, 0)
		for _, opt := range strings.Split(transcodingOptions, ",") {
			p, ok := ffmpeg.VideoProfileLookup[strings.TrimSpace(opt)]
			if ok {
				bProfiles = append(bProfiles, p)
			}
		}
		BroadcastJobVideoProfiles = bProfiles
		glog.V(common.SHORT).Infof("Transcode Job Price: %v, Transcode Job Type: %v", BroadcastPrice, BroadcastJobVideoProfiles)
	}

	//LPMS handlers for handling RTMP video
	s.LPMS.HandleRTMPPublish(createRTMPStreamIDHandler(s), gotRTMPStreamHandler(s), endRTMPStreamHandler(s))
	s.LPMS.HandleRTMPPlay(getRTMPStreamHandler(s))

	//LPMS hanlder for handling HLS video play
	s.LPMS.HandleHLSPlay(getHLSMasterPlaylistHandler(s), getHLSMediaPlaylistHandler(s), getHLSSegmentHandler(s))

	//Start the LPMS server
	lpmsCtx, cancel := context.WithCancel(context.Background())
	ec := make(chan error, 1)
	go func() {
		if err := s.LPMS.Start(lpmsCtx); err != nil {
			// typically triggered if there's an error with broadcaster LPMS
			// transcoder LPMS should return without an error
			ec <- s.LPMS.Start(lpmsCtx)
		}
	}()

	select {
	case err := <-ec:
		glog.Infof("LPMS Server Error: %v.  Quitting...", err)
		cancel()
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

//RTMP Publish Handlers
func createRTMPStreamIDHandler(s *LivepeerServer) func(url *url.URL) (strmID string) {
	return func(url *url.URL) (strmID string) {
		//Create a StreamID
		//If streamID is passed in, use that one
		//Else create one
		mid := parseManifestID(url.Query().Get("manifestID"))
		if mid == "" {
			mid = core.RandomManifestID()
		}

		// Ensure there's no concurrent StreamID with the same name
		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		if _, exists := s.rtmpConnections[mid]; exists {
			glog.Error("Manifest already exists ", mid)
			return ""
		}

		// Generate RTMP part of StreamID
		key := hex.EncodeToString(core.RandomIdGenerator(StreamKeyBytes))
		return core.MakeStreamIDFromString(string(mid), key).String()
	}

}

func (s *LivepeerServer) startBroadcast(cpl core.PlaylistManager) (*BroadcastSession, error) {

	if s.LivepeerNode.OrchestratorPool == nil {
		glog.Info("No orchestrators specified; not transcoding")
		return nil, ErrDiscovery
	}

	rpcBcast := core.NewBroadcaster(s.LivepeerNode)

	tinfos, err := s.LivepeerNode.OrchestratorPool.GetOrchestrators(1)
	if len(tinfos) <= 0 || err != nil {
		return nil, err
	}
	tinfo := tinfos[0]

	// set OSes
	var orchOS drivers.OSSession
	if len(tinfo.Storage) > 0 {
		orchOS = drivers.NewSession(tinfo.Storage[0])
	}

	return &BroadcastSession{
		Broadcaster:      rpcBcast,
		ManifestID:       cpl.ManifestID(),
		Profiles:         BroadcastJobVideoProfiles,
		OrchestratorInfo: tinfo,
		OrchestratorOS:   orchOS,
		BroadcasterOS:    cpl.GetOSSession(),
	}, nil
}

func rtmpManifestID(rtmpStrm stream.RTMPVideoStream) core.ManifestID {
	return parseManifestID(rtmpStrm.GetStreamID())
}

func gotRTMPStreamHandler(s *LivepeerServer) func(url *url.URL, rtmpStrm stream.RTMPVideoStream) (err error) {
	return func(url *url.URL, rtmpStrm stream.RTMPVideoStream) (err error) {

		// Set up the connection tracking
		mid := rtmpManifestID(rtmpStrm)
		if drivers.NodeStorage == nil {
			glog.Error("Missing node storage")
			return ErrStorage
		}
		storage := drivers.NodeStorage.NewSession(string(mid))
		// Build the source video profile from the RTMP stream.
		resolution := fmt.Sprintf("%vx%v", rtmpStrm.Width(), rtmpStrm.Height())
		vProfile := ffmpeg.VideoProfile{
			Name:       "source",
			Resolution: resolution,
			Bitrate:    "4000k", // Fix this
		}
		s.connectionLock.Lock()
		_, exists := s.rtmpConnections[mid]
		if exists {
			// We can only have one concurrent stream per ManifestID
			s.connectionLock.Unlock()
			return ErrAlreadyExists
		}
		nonce := rand.Uint64()
		cxn := &rtmpConnection{
			nonce:   nonce,
			stream:  rtmpStrm,
			pl:      core.NewBasicPlaylistManager(mid, storage),
			profile: &vProfile,
		}
		s.rtmpConnections[mid] = cxn
		s.connectionLock.Unlock()
		LastManifestID = mid

		startSeq := 0
		cpl := cxn.pl
		var sess *BroadcastSession

		if s.LivepeerNode.Eth != nil {

			//Check if round is initialized
			initialized, err := s.LivepeerNode.Eth.CurrentRoundInitialized()
			if err != nil {
				glog.Errorf("Could not check whether round was initialized: %v", err)
				return err
			}
			if !initialized {
				glog.Infof("Round was uninitialized. Please try again in a few blocks.")
				// todo send to metrics ?
				return ErrRoundInit
			}
			// Check deposit
			deposit, err := s.LivepeerNode.Eth.BroadcasterDeposit(s.LivepeerNode.Eth.Account().Address)
			if err != nil {
				glog.Errorf("Error getting deposit: %v", err)
				return ErrBroadcast
			}

			glog.V(common.SHORT).Infof("Current deposit is: %v", deposit)

			minDeposit := big.NewInt(0).Mul(BroadcastPrice, big.NewInt(MinDepositSegmentCount))
			if deposit.Cmp(minDeposit) < 0 {
				glog.Errorf("Low deposit (%v) - cannot start broadcast session.  Need at least %v", deposit, minDeposit)
				if monitor.Enabled {
					monitor.LogStreamCreateFailed(nonce, "LowDeposit")
				}
				return ErrBroadcast
			}
		}

		hlsStrmID := core.MakeStreamID(mid, &vProfile)

		LastHLSStreamID = hlsStrmID

		streamStarted := false
		//Segment the stream, insert the segments into the broadcaster
		go func(rtmpStrm stream.RTMPVideoStream) {
			hid := string(core.RandomManifestID()) // ffmpeg m3u8 output name
			hlsStrm := stream.NewBasicHLSVideoStream(hid, stream.DefaultHLSStreamWin)
			hlsStrm.SetSubscriber(func(seg *stream.HLSSegment, eof bool) {
				if eof {
					// XXX update HLS manifest
					return
				}
				if streamStarted == false {
					streamStarted = true
					if monitor.Enabled {
						monitor.LogStreamStartedEvent(nonce)
					}
				}
				processSegment(cxn, seg)
			})

			segOptions := segmenter.SegmenterOptions{
				StartSeq:  startSeq,
				SegLength: SegLen,
			}
			err := s.RTMPSegmenter.SegmentRTMPToHLS(context.Background(), rtmpStrm, hlsStrm, segOptions)
			if err != nil {
				// Stop the incoming RTMP connection.
				// TODO retry segmentation if err != SegmenterTimeout; may be recoverable
				rtmpStrm.Close()
			}

		}(rtmpStrm)

		if monitor.Enabled {
			monitor.LogStreamCreatedEvent(string(mid), nonce)
		}

		glog.Infof("\n\nVideo Created With ManifestID: %v\n\n", mid)
		glog.V(common.SHORT).Infof("\n\nhlsStrmID: %v\n\n", hlsStrmID)

		//Create Transcode Job Onchain
		go func() {
			// Connect to the orchestrator. If it fails, retry for as long
			// as the RTMP stream is alive; maybe the orchestrator hasn't
			// received the block containing the job yet
			broadcastFunc := func() error {
				sess, err = s.startBroadcast(cpl)
				if err == ErrDiscovery {
					return nil // discovery disabled, don't retry
				} else if err != nil {
					// Should be logged upstream
				}
				s.connectionLock.Lock()
				defer s.connectionLock.Unlock()
				cxn, active := s.rtmpConnections[mid]
				if active {
					cxn.sess = sess
					return err
				}
				return nil // stop if inactive
			}
			expb := backoff.NewExponentialBackOff()
			expb.MaxInterval = BroadcastRetry
			expb.MaxElapsedTime = 0
			backoff.Retry(broadcastFunc, expb)
		}()
		return nil
	}
}

func endRTMPStreamHandler(s *LivepeerServer) func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error {
	return func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error {
		mid := rtmpManifestID(rtmpStrm)
		//Remove RTMP stream
		s.connectionLock.Lock()
		defer s.connectionLock.Unlock()
		cxn, ok := s.rtmpConnections[mid]
		if !ok || cxn.pl == nil {
			glog.Error("Attempted to end unknown stream with manifest ID ", mid)
			return ErrUnknownStream
		}
		cxn.pl.Cleanup()
		if monitor.Enabled {
			monitor.LogStreamEndedEvent(cxn.nonce)
		}
		delete(s.rtmpConnections, mid)

		return nil
	}
}

//End RTMP Publish Handlers

//HLS Play Handlers
func getHLSMasterPlaylistHandler(s *LivepeerServer) func(url *url.URL) (*m3u8.MasterPlaylist, error) {
	return func(url *url.URL) (*m3u8.MasterPlaylist, error) {
		var manifestID core.ManifestID
		if s.ExposeCurrentManifest && "/stream/current.m3u8" == strings.ToLower(url.Path) {
			manifestID = LastManifestID
		} else {
			sid := parseStreamID(url.Path)
			if sid.Rendition != "" {
				// requesting a media PL, not master PL
				return nil, vidplayer.ErrNotFound
			}
			manifestID = sid.ManifestID
		}

		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		cxn, ok := s.rtmpConnections[manifestID]
		if !ok || cxn.pl == nil {
			return nil, vidplayer.ErrNotFound
		}
		cpl := cxn.pl

		if cpl.ManifestID() != manifestID {
			return nil, vidplayer.ErrNotFound
		}
		return cpl.GetHLSMasterPlaylist(), nil
	}
}

func getHLSMediaPlaylistHandler(s *LivepeerServer) func(url *url.URL) (*m3u8.MediaPlaylist, error) {
	return func(url *url.URL) (*m3u8.MediaPlaylist, error) {
		strmID := parseStreamID(url.Path)
		mid := strmID.ManifestID
		s.connectionLock.RLock()
		defer s.connectionLock.RUnlock()
		cxn, ok := s.rtmpConnections[mid]
		if !ok || cxn.pl == nil {
			return nil, vidplayer.ErrNotFound
		}

		//Get the hls playlist
		pl := cxn.pl.GetHLSMediaPlaylist(strmID.Rendition)
		if pl == nil {
			return nil, vidplayer.ErrNotFound
		}
		return pl, nil
	}
}

func getHLSSegmentHandler(s *LivepeerServer) func(url *url.URL) ([]byte, error) {
	return func(url *url.URL) ([]byte, error) {
		// Strip the /stream/ prefix
		segName := cleanStreamPrefix(url.Path)
		if segName == "" || drivers.NodeStorage == nil {
			glog.Error("SegName not found or storage nil")
			return nil, vidplayer.ErrNotFound
		}
		parts := strings.SplitN(segName, "/", 2)
		if len(parts) <= 0 {
			glog.Error("Unexpected path structure")
			return nil, vidplayer.ErrNotFound
		}
		memoryOS, ok := drivers.NodeStorage.(*drivers.MemoryOS)
		if !ok {
			return nil, vidplayer.ErrNotFound
		}
		// We index the session by the first entry of the path, eg
		// <session>/<more-path>/<data>
		os := memoryOS.GetSession(parts[0])
		if os == nil {
			return nil, vidplayer.ErrNotFound
		}
		data := os.GetData(segName)
		if len(data) > 0 {
			return data, nil
		}
		return nil, vidplayer.ErrNotFound
	}
}

//End HLS Play Handlers

//Start RTMP Play Handlers
func getRTMPStreamHandler(s *LivepeerServer) func(url *url.URL) (stream.RTMPVideoStream, error) {
	return func(url *url.URL) (stream.RTMPVideoStream, error) {
		mid := parseManifestID(url.Path)
		s.connectionLock.RLock()
		cxn, ok := s.rtmpConnections[mid]
		defer s.connectionLock.RUnlock()
		if !ok {
			glog.Error("Cannot find RTMP stream for ManifestID ", mid)
			return nil, vidplayer.ErrNotFound
		}

		//Could use a subscriber, but not going to here because the RTMP stream doesn't need to be available for consumption by multiple views.  It's only for the segmenter.
		return cxn.stream, nil
	}
}

//End RTMP Handlers

//Helper Methods Begin

// Match all leading spaces, slashes and optionally `stream/`
var StreamPrefix = regexp.MustCompile(`^[ /]*(stream/)?`)

func cleanStreamPrefix(reqPath string) string {
	return StreamPrefix.ReplaceAllString(reqPath, "")
}

func parseStreamID(reqPath string) core.StreamID {
	// remove extension and create streamid
	p := strings.TrimSuffix(reqPath, path.Ext(reqPath))
	return core.SplitStreamIDString(cleanStreamPrefix(p))
}

func parseManifestID(reqPath string) core.ManifestID {
	return parseStreamID(reqPath).ManifestID
}

func (s *LivepeerServer) GetNodeStatus() *net.NodeStatus {
	// not threadsafe; need to deep copy the playlist
	m := make(map[string]*m3u8.MasterPlaylist, 0)

	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	for _, cxn := range s.rtmpConnections {
		if cxn.sess == nil || cxn.pl == nil {
			continue
		}
		cpl := cxn.pl
		m[string(cpl.ManifestID())] = cpl.GetHLSMasterPlaylist()
	}
	return &net.NodeStatus{Manifests: m}
}

// Debug helpers
func (s *LivepeerServer) LatestPlaylist() core.PlaylistManager {
	s.connectionLock.RLock()
	defer s.connectionLock.RUnlock()
	cxn, ok := s.rtmpConnections[LastManifestID]
	if !ok || cxn.pl == nil {
		return nil
	}
	return cxn.pl
}

func shouldStopStream(err error) bool {
	return false
}
