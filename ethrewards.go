package ethrewards

import (
	"fmt"
	"sync"
	"time"

	"github.com/DillLabs/dillscan-rewards/src/common/log"
	"github.com/DillLabs/eth-rewards/beacon"
	"github.com/DillLabs/eth-rewards/elrewards"
	"github.com/DillLabs/eth-rewards/types"
	"golang.org/x/sync/errgroup"

	"github.com/sirupsen/logrus"
)

func GetRewardsForEpoch(epoch uint64, client *beacon.Client, elEndpoint string) (map[uint64]*types.ValidatorEpochIncome, error) {
	startTime0 := time.Now()
	proposerAssignments, err := client.ProposerAssignments(epoch) //web
	endTime0 := time.Now()
	duration0 := endTime0.Sub(startTime0).Seconds()
	log.Debug("WEB ExecutionProposerAssignments duration", "duration", duration0)
	if err != nil {
		return nil, err
	}

	slotsPerEpoch := uint64(len(proposerAssignments.Data))

	startSlot := epoch * slotsPerEpoch
	endSlot := startSlot + slotsPerEpoch - 1

	g := new(errgroup.Group)
	g.SetLimit(32)

	slotsToProposerIndex := make(map[uint64]uint64)
	for _, pa := range proposerAssignments.Data {
		slotsToProposerIndex[uint64(pa.Slot)] = uint64(pa.ValidatorIndex)
	}

	rewardsMux := &sync.Mutex{}
	rewards := make(map[uint64]*types.ValidatorEpochIncome)

	for i := startSlot; i <= endSlot; i++ {
		i := i

		g.Go(func() error {

			proposer, found := slotsToProposerIndex[i]
			if !found {
				return fmt.Errorf("assigned proposer for slot %v not found", i)
			}
			startTime1 := time.Now()
			execBlockNumber, err := client.ExecutionBlockNumber(i) // web
			endTime1 := time.Now()
			duration1 := endTime1.Sub(startTime1).Seconds()
			log.Debug("WEB ExecutionBlockNumber duration", "slot", i, "duration", duration1)

			rewardsMux.Lock()
			if rewards[proposer] == nil {
				rewards[proposer] = &types.ValidatorEpochIncome{}
			}
			rewardsMux.Unlock()
			if err != nil {
				if err == types.ErrBlockNotFound {
					rewardsMux.Lock()
					rewards[proposer].ProposalsMissed += 1
					rewardsMux.Unlock()
					return nil
				} else if err != types.ErrSlotPreMerge { // ignore
					logrus.Errorf("error retrieving execution block number for slot %v: %v", i, err)
					return err
				}
			} else {

				startTime2 := time.Now()
				txFeeIncome, err := elrewards.GetELRewardForBlock(execBlockNumber, elEndpoint) //web
				endTime2 := time.Now()
				duration2 := endTime2.Sub(startTime2).Seconds()
				log.Debug("WEB ExecutionGetELRewardForBlock duration", "slot", i, "duration", duration2)

				if err != nil {
					log.Info("error retrieving EL reward for block ", "execBlockNumber", execBlockNumber, "err", err)
					return err
				}

				rewardsMux.Lock()
				rewards[proposer].TxFeeRewardWei = txFeeIncome.Bytes()
				rewardsMux.Unlock()
			}
			// startTime3 := time.Now()
			syncRewards, err := client.SyncCommitteeRewards(i) //web
			// endTime3 := time.Now()
			// duration3 := endTime3.Sub(startTime3).Seconds()
			// log.Info("WEB ExecutionSyncCommitteeRewards duration", "slot", i, "duration", duration3)
			if err != nil {
				if err != types.ErrSlotPreSyncCommittees {
					return err
				}
			}
			// if syncRewards != nil {
			// 	log.Info("SyncCommitteeRewards result", "slot", i, "result", syncRewards)
			// } else {
			// 	log.Info("SyncCommitteeRewards returned nil", "slot", i)
			// }

			rewardsMux.Lock()
			if syncRewards != nil {
				for _, sr := range syncRewards.Data {
					if rewards[sr.ValidatorIndex] == nil {
						rewards[sr.ValidatorIndex] = &types.ValidatorEpochIncome{}
					}

					if sr.Reward > 0 {
						rewards[sr.ValidatorIndex].SyncCommitteeReward += uint64(sr.Reward)
					} else {
						rewards[sr.ValidatorIndex].SyncCommitteePenalty += uint64(sr.Reward * -1)
					}
				}
			}
			rewardsMux.Unlock()

			// startTime4 := time.Now()
			blockRewards, err := client.BlockRewards(i) //web
			// endTime4 := time.Now()
			// duration4 := endTime4.Sub(startTime4).Seconds()
			// log.Info("WEB BlockRewards duration", "slot", i, "duration", duration4)
			if err != nil {
				// rewardsMux.Unlock()
				return err
			}
			rewardsMux.Lock()
			if rewards[blockRewards.Data.ProposerIndex] == nil {
				rewards[blockRewards.Data.ProposerIndex] = &types.ValidatorEpochIncome{}
			}
			rewards[blockRewards.Data.ProposerIndex].ProposerAttestationInclusionReward += blockRewards.Data.Attestations
			rewards[blockRewards.Data.ProposerIndex].ProposerSlashingInclusionReward += blockRewards.Data.AttesterSlashings + blockRewards.Data.ProposerSlashings
			rewards[blockRewards.Data.ProposerIndex].ProposerSyncInclusionReward += blockRewards.Data.SyncAggregate
			rewardsMux.Unlock()

			return nil
		})
	}

	g.Go(func() error {
		startTime5 := time.Now()
		ar, err := client.AttestationRewards(epoch) //web
		endTime5 := time.Now()
		duration5 := endTime5.Sub(startTime5).Seconds()
		log.Info("WEB AttestationRewards duration", "duration", duration5)
		if err != nil {
			return err
		}
		rewardsMux.Lock()
		defer rewardsMux.Unlock()
		for _, ar := range ar.Data.TotalRewards {
			if rewards[ar.ValidatorIndex] == nil {
				rewards[ar.ValidatorIndex] = &types.ValidatorEpochIncome{}
			}

			if ar.Head >= 0 {
				rewards[ar.ValidatorIndex].AttestationHeadReward = uint64(ar.Head)
			} else {
				return fmt.Errorf("retrieved negative attestation head reward for validator %v: %v", ar.ValidatorIndex, ar.Head)
			}

			if ar.Source > 0 {
				rewards[ar.ValidatorIndex].AttestationSourceReward = uint64(ar.Source)
			} else {
				rewards[ar.ValidatorIndex].AttestationSourcePenalty = uint64(ar.Source * -1)
			}

			if ar.Target > 0 {
				rewards[ar.ValidatorIndex].AttestationTargetReward = uint64(ar.Target)
			} else {
				rewards[ar.ValidatorIndex].AttestationTargetPenalty = uint64(ar.Target * -1)
			}

			if ar.InclusionDelay <= 0 {
				rewards[ar.ValidatorIndex].FinalityDelayPenalty = uint64(ar.InclusionDelay * -1)
			} else {
				return fmt.Errorf("retrieved positive inclusion delay penalty for validator %v: %v", ar.ValidatorIndex, ar.InclusionDelay)
			}
		}

		return nil
	})

	err = g.Wait()
	if err != nil {
		return nil, err
	}

	return rewards, nil
}
