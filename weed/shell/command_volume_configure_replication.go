package shell

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/chrislusf/seaweedfs/weed/pb"
	"io"
	"path/filepath"

	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/master_pb"
	"github.com/chrislusf/seaweedfs/weed/pb/volume_server_pb"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
	"github.com/chrislusf/seaweedfs/weed/storage/super_block"
)

func init() {
	Commands = append(Commands, &commandVolumeConfigureReplication{})
}

type commandVolumeConfigureReplication struct {
}

func (c *commandVolumeConfigureReplication) Name() string {
	return "volume.configure.replication"
}

func (c *commandVolumeConfigureReplication) Help() string {
	return `change volume replication value

	This command changes a volume replication value. It should be followed by "volume.fix.replication".

`
}

func (c *commandVolumeConfigureReplication) Do(args []string, commandEnv *CommandEnv, _ io.Writer) (err error) {

	configureReplicationCommand := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	volumeIdInt := configureReplicationCommand.Int("volumeId", 0, "the volume id")
	replicationString := configureReplicationCommand.String("replication", "", "the intended replication value")
	collectionPattern := configureReplicationCommand.String("collectionPattern", "", "match with wildcard characters '*' and '?'")
	if err = configureReplicationCommand.Parse(args); err != nil {
		return nil
	}

	if err = commandEnv.confirmIsLocked(); err != nil {
		return
	}

	if *replicationString == "" {
		return fmt.Errorf("empty replication value")
	}

	replicaPlacement, err := super_block.NewReplicaPlacementFromString(*replicationString)
	if err != nil {
		return fmt.Errorf("replication format: %v", err)
	}

	// collect topology information
	topologyInfo, _, err := collectTopologyInfo(commandEnv)
	if err != nil {
		return err
	}

	vid := needle.VolumeId(*volumeIdInt)
	volumeFilter := getVolumeFilter(replicaPlacement, uint32(vid), *collectionPattern)

	// find all data nodes with volumes that needs replication change
	var allLocations []location
	eachDataNode(topologyInfo, func(dc string, rack RackId, dn *master_pb.DataNodeInfo) {
		loc := newLocation(dc, string(rack), dn)
		for _, diskInfo := range dn.DiskInfos {
			for _, v := range diskInfo.VolumeInfos {
				if volumeFilter(v) {
					allLocations = append(allLocations, loc)
					continue
				}
			}
		}
	})

	if len(allLocations) == 0 {
		return fmt.Errorf("no volume needs change")
	}

	for _, dst := range allLocations {
		err := operation.WithVolumeServerClient(pb.NewServerAddressFromDataNode(dst.dataNode), commandEnv.option.GrpcDialOption, func(volumeServerClient volume_server_pb.VolumeServerClient) error {
			resp, configureErr := volumeServerClient.VolumeConfigure(context.Background(), &volume_server_pb.VolumeConfigureRequest{
				VolumeId:    uint32(vid),
				Replication: replicaPlacement.String(),
			})
			if configureErr != nil {
				return configureErr
			}
			if resp.Error != "" {
				return errors.New(resp.Error)
			}
			return nil
		})

		if err != nil {
			return err
		}

	}

	return nil
}

func getVolumeFilter(replicaPlacement *super_block.ReplicaPlacement, volumeId uint32, collectionPattern string) func(message *master_pb.VolumeInformationMessage) bool {
	replicaPlacementInt32 := uint32(replicaPlacement.Byte())
	if volumeId > 0 {
		return func(v *master_pb.VolumeInformationMessage) bool {
			return v.Id == volumeId && v.ReplicaPlacement != replicaPlacementInt32
		}
	}
	return func(v *master_pb.VolumeInformationMessage) bool {
		matched, err := filepath.Match(collectionPattern, v.Collection)
		if err != nil {
			return false
		}
		return matched
	}
}
