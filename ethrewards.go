package ethrewards

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/DillLabs/eth-rewards/beacon"
	"github.com/DillLabs/eth-rewards/types"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"golang.org/x/sync/errgroup"

	"github.com/sirupsen/logrus"
)

func GetRewardsForEpoch(epoch uint64, client *beacon.Client, elEndpoint string) (map[uint64]*types.ValidatorEpochIncome, error) {
	proposerAssignments, err := client.ProposerAssignments(epoch)
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
	amountMux := &sync.Mutex{}

	rewards := make(map[uint64]*types.ValidatorEpochIncome)
	mapValidatorIndexWithdrawalAmount := make(map[uint64]uint64)

	for i := startSlot; i <= endSlot; i++ {
		i := i

		g.Go(func() error {
			proposer, found := slotsToProposerIndex[i]
			if !found {
				return fmt.Errorf("assigned proposer for slot %v not found", i)
			}

			execBlockNumber, err := client.ExecutionBlockNumber(i)
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
				ethBlock, err := GetExecutionBlock(execBlockNumber, elEndpoint)
				if err != nil {
					return err
				}
				if len(ethBlock.Withdrawals()) > 0 {
					for _, wd := range ethBlock.Withdrawals() {
						if wd.Amount > 0 {
							//logrus.Debugf("wd.Validator %d, wd.Amount %d", wd.Validator, wd.Amount)
							amountMux.Lock()
							if mapValidatorIndexWithdrawalAmount[wd.Validator] == 0 {
								mapValidatorIndexWithdrawalAmount[wd.Validator] = wd.Amount
							} else {
								mapValidatorIndexWithdrawalAmount[wd.Validator] += wd.Amount
							}
							amountMux.Unlock()
						}
					}
				}

				//txFeeIncome, err := elrewards.GetELRewardForBlock(execBlockNumber, elEndpoint)
				//if err != nil {
				//	return err
				//}

				//rewardsMux.Lock()
				//rewards[proposer].TxFeeRewardWei = txFeeIncome.Bytes()
				//rewardsMux.Unlock()
			}

			syncRewards, err := client.SyncCommitteeRewards(i)
			if err != nil {
				if err != types.ErrSlotPreSyncCommittees {
					return err
				}
			}

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

			blockRewards, err := client.BlockRewards(i)
			if err != nil {
				rewardsMux.Unlock()
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
		ar, err := client.AttestationRewards(epoch)
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

	for index, amount := range mapValidatorIndexWithdrawalAmount {
		if amount > 0 {
			//logrus.Debugf("index %d, amount %d", index, amount)
			rewardsMux.Lock()
			if rewards[index] == nil {
				rewards[index] = &types.ValidatorEpochIncome{}
			}
			rewards[index].WithdrawalAmount = amount
			rewardsMux.Unlock()
		}
	}

	return rewards, nil
}

func GetExecutionBlock(executionBlockNumber uint64, endpoint string) (*ethTypes.Block, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	nativeClient, err := ethclient.Dial(endpoint)
	if err != nil {
		return nil, err
	}

	block, err := nativeClient.BlockByNumber(ctx, big.NewInt(int64(executionBlockNumber)))
	if err != nil {
		return nil, err
	}
	return block, nil
}
