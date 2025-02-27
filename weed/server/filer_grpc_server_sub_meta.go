package weed_server

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"

	"github.com/chrislusf/seaweedfs/weed/filer"
	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/util"
	"github.com/chrislusf/seaweedfs/weed/util/log_buffer"
)

const (
	// MaxUnsyncedEvents send empty notification with timestamp when certain amount of events have been filtered
	MaxUnsyncedEvents = 1e3
)

func (fs *FilerServer) SubscribeMetadata(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeMetadataServer) error {

	peerAddress := findClientAddress(stream.Context(), 0)

	clientName := fs.addClient(req.ClientName, peerAddress)

	defer fs.deleteClient(clientName)

	lastReadTime := time.Unix(0, req.SinceNs)
	glog.V(0).Infof(" %v starts to subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)

	eachEventNotificationFn := fs.eachEventNotificationFn(req, stream, clientName)

	eachLogEntryFn := eachLogEntryFn(eachEventNotificationFn)

	var processedTsNs int64
	var readPersistedLogErr error
	var readInMemoryLogErr error

	for {

		glog.V(4).Infof("read on disk %v aggregated subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)

		processedTsNs, readPersistedLogErr = fs.filer.ReadPersistedLogBuffer(lastReadTime, eachLogEntryFn)
		if readPersistedLogErr != nil {
			return fmt.Errorf("reading from persisted logs: %v", readPersistedLogErr)
		}

		if processedTsNs != 0 {
			lastReadTime = time.Unix(0, processedTsNs)
		}

		glog.V(4).Infof("read in memory %v aggregated subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)

		lastReadTime, readInMemoryLogErr = fs.filer.MetaAggregator.MetaLogBuffer.LoopProcessLogData("aggMeta:"+clientName, lastReadTime, func() bool {
			fs.filer.MetaAggregator.ListenersLock.Lock()
			fs.filer.MetaAggregator.ListenersCond.Wait()
			fs.filer.MetaAggregator.ListenersLock.Unlock()
			return true
		}, eachLogEntryFn)
		if readInMemoryLogErr != nil {
			if readInMemoryLogErr == log_buffer.ResumeFromDiskError {
				time.Sleep(1127 * time.Millisecond)
				continue
			}
			glog.Errorf("processed to %v: %v", lastReadTime, readInMemoryLogErr)
			if readInMemoryLogErr != log_buffer.ResumeError {
				break
			}
		}

		time.Sleep(1127 * time.Millisecond)
	}

	return readInMemoryLogErr

}

func (fs *FilerServer) SubscribeLocalMetadata(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeLocalMetadataServer) error {

	peerAddress := findClientAddress(stream.Context(), 0)

	clientName := fs.addClient(req.ClientName, peerAddress)

	defer fs.deleteClient(clientName)

	lastReadTime := time.Unix(0, req.SinceNs)
	glog.V(0).Infof(" %v local subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)

	eachEventNotificationFn := fs.eachEventNotificationFn(req, stream, clientName)

	eachLogEntryFn := eachLogEntryFn(eachEventNotificationFn)

	var processedTsNs int64
	var readPersistedLogErr error
	var readInMemoryLogErr error

	for {
		// println("reading from persisted logs ...")
		glog.V(0).Infof("read on disk %v local subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)
		processedTsNs, readPersistedLogErr = fs.filer.ReadPersistedLogBuffer(lastReadTime, eachLogEntryFn)
		if readPersistedLogErr != nil {
			glog.V(0).Infof("read on disk %v local subscribe %s from %+v: %v", clientName, req.PathPrefix, lastReadTime, readPersistedLogErr)
			return fmt.Errorf("reading from persisted logs: %v", readPersistedLogErr)
		}

		if processedTsNs != 0 {
			lastReadTime = time.Unix(0, processedTsNs)
		} else {
			if readInMemoryLogErr == log_buffer.ResumeFromDiskError {
				time.Sleep(1127 * time.Millisecond)
				continue
			}
		}

		glog.V(0).Infof("read in memory %v local subscribe %s from %+v", clientName, req.PathPrefix, lastReadTime)

		lastReadTime, readInMemoryLogErr = fs.filer.LocalMetaLogBuffer.LoopProcessLogData("localMeta:"+clientName, lastReadTime, func() bool {
			fs.listenersLock.Lock()
			fs.listenersCond.Wait()
			fs.listenersLock.Unlock()
			return true
		}, eachLogEntryFn)
		if readInMemoryLogErr != nil {
			if readInMemoryLogErr == log_buffer.ResumeFromDiskError {
				continue
			}
			glog.Errorf("processed to %v: %v", lastReadTime, readInMemoryLogErr)
			time.Sleep(1127 * time.Millisecond)
			if readInMemoryLogErr != log_buffer.ResumeError {
				break
			}
		}
	}

	return readInMemoryLogErr

}

func eachLogEntryFn(eachEventNotificationFn func(dirPath string, eventNotification *filer_pb.EventNotification, tsNs int64) error) func(logEntry *filer_pb.LogEntry) error {
	return func(logEntry *filer_pb.LogEntry) error {
		event := &filer_pb.SubscribeMetadataResponse{}
		if err := proto.Unmarshal(logEntry.Data, event); err != nil {
			glog.Errorf("unexpected unmarshal filer_pb.SubscribeMetadataResponse: %v", err)
			return fmt.Errorf("unexpected unmarshal filer_pb.SubscribeMetadataResponse: %v", err)
		}

		if err := eachEventNotificationFn(event.Directory, event.EventNotification, event.TsNs); err != nil {
			return err
		}

		return nil
	}
}

func (fs *FilerServer) eachEventNotificationFn(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeMetadataServer, clientName string) func(dirPath string, eventNotification *filer_pb.EventNotification, tsNs int64) error {
	filtered := 0

	return func(dirPath string, eventNotification *filer_pb.EventNotification, tsNs int64) error {
		defer func() {
			if filtered > MaxUnsyncedEvents {
				if err := stream.Send(&filer_pb.SubscribeMetadataResponse{
					EventNotification: &filer_pb.EventNotification{},
					TsNs:              tsNs,
				}); err == nil {
					filtered = 0
				}
			}
		}()

		filtered++
		foundSelf := false
		for _, sig := range eventNotification.Signatures {
			if sig == req.Signature && req.Signature != 0 {
				return nil
			}
			if sig == fs.filer.Signature {
				foundSelf = true
			}
		}
		if !foundSelf {
			eventNotification.Signatures = append(eventNotification.Signatures, fs.filer.Signature)
		}

		// get complete path to the file or directory
		var entryName string
		if eventNotification.OldEntry != nil {
			entryName = eventNotification.OldEntry.Name
		} else if eventNotification.NewEntry != nil {
			entryName = eventNotification.NewEntry.Name
		}

		fullpath := util.Join(dirPath, entryName)

		// skip on filer internal meta logs
		if strings.HasPrefix(fullpath, filer.SystemLogDir) {
			return nil
		}

		if hasPrefixIn(fullpath, req.PathPrefixes) {
			// good
		} else {
			if !strings.HasPrefix(fullpath, req.PathPrefix) {
				if eventNotification.NewParentPath != "" {
					newFullPath := util.Join(eventNotification.NewParentPath, entryName)
					if !strings.HasPrefix(newFullPath, req.PathPrefix) {
						return nil
					}
				} else {
					return nil
				}
			}
		}

		message := &filer_pb.SubscribeMetadataResponse{
			Directory:         dirPath,
			EventNotification: eventNotification,
			TsNs:              tsNs,
		}
		// println("sending", dirPath, entryName)
		if err := stream.Send(message); err != nil {
			glog.V(0).Infof("=> client %v: %+v", clientName, err)
			return err
		}
		filtered = 0
		return nil
	}
}

func hasPrefixIn(text string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func (fs *FilerServer) addClient(clientType string, clientAddress string) (clientName string) {
	clientName = clientType + "@" + clientAddress
	glog.V(0).Infof("+ listener %v", clientName)
	return
}

func (fs *FilerServer) deleteClient(clientName string) {
	glog.V(0).Infof("- listener %v", clientName)
}
