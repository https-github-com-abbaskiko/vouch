// Copyright © 2020 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package best

import (
	"context"
	"sync"
	"time"

	eth2client "github.com/attestantio/go-eth2-client"
	spec "github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/attestantio/vouch/services/metrics"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	zerologger "github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

// Service is the provider for beacon block proposals.
type Service struct {
	clientMonitor                metrics.ClientMonitor
	processConcurrency           int64
	beaconBlockProposalProviders map[string]eth2client.BeaconBlockProposalProvider
	timeout                      time.Duration
}

// module-wide log.
var log zerolog.Logger

// New creates a new beacon block propsal strategy.
func New(ctx context.Context, params ...Parameter) (*Service, error) {
	parameters, err := parseAndCheckParameters(params...)
	if err != nil {
		return nil, errors.Wrap(err, "problem with parameters")
	}

	// Set logging.
	log = zerologger.With().Str("strategy", "beaconblockproposal").Str("impl", "best").Logger()
	if parameters.logLevel != log.GetLevel() {
		log = log.Level(parameters.logLevel)
	}

	s := &Service{
		processConcurrency:           parameters.processConcurrency,
		beaconBlockProposalProviders: parameters.beaconBlockProposalProviders,
		timeout:                      parameters.timeout,
		clientMonitor:                parameters.clientMonitor,
	}

	return s, nil
}

// BeaconBlockProposal provies the best beacon block proposal from a number of beacon nodes.
func (s *Service) BeaconBlockProposal(ctx context.Context, slot uint64, randaoReveal []byte, graffiti []byte) (*spec.BeaconBlock, error) {
	var mu sync.Mutex
	bestScore := float64(0)
	var bestProposal *spec.BeaconBlock

	sem := semaphore.NewWeighted(s.processConcurrency)
	var wg sync.WaitGroup
	for name, provider := range s.beaconBlockProposalProviders {
		wg.Add(1)
		go func(ctx context.Context, sem *semaphore.Weighted, wg *sync.WaitGroup, name string, provider eth2client.BeaconBlockProposalProvider, mu *sync.Mutex) {
			defer wg.Done()

			if err := sem.Acquire(ctx, 1); err != nil {
				log.Error().Err(err).Msg("Failed to acquire semaphore")
				return
			}
			log := log.With().Str("provider", name).Uint64("slot", slot).Logger()

			opCtx, cancel := context.WithTimeout(ctx, s.timeout)
			started := time.Now()
			proposal, err := provider.BeaconBlockProposal(opCtx, slot, randaoReveal, graffiti)
			s.clientMonitor.ClientOperation(name, "beacon block proposal", err == nil, time.Since(started))
			if err != nil {
				log.Warn().Err(err).Msg("Failed to obtain beacon block proposal")
				cancel()
				return
			}
			log.Trace().Dur("elapsed", time.Since(started)).Msg("Obtained beacon block proposal")
			cancel()

			mu.Lock()
			score := scoreBeaconBlockProposal(ctx, name, slot, proposal)
			if score > bestScore || bestProposal == nil {
				bestScore = score
				bestProposal = proposal
			}
			mu.Unlock()
		}(ctx, sem, &wg, name, provider, &mu)
	}
	wg.Wait()

	return bestProposal, nil
}

// scoreBeaconBlockPropsal generates a score for a beacon block.
// The score is relative to the reward expected by proposing the block.
func scoreBeaconBlockProposal(ctx context.Context, name string, slot uint64, blockProposal *spec.BeaconBlock) float64 {
	immediateAttestationScore := float64(0)
	attestationScore := float64(0)

	// Add attestation scores.
	for _, attestation := range blockProposal.Body.Attestations {
		inclusionDistance := float64(slot - attestation.Data.Slot)
		attestationScore += float64(attestation.AggregationBits.Count()) / inclusionDistance
		if inclusionDistance == 1 {
			immediateAttestationScore += float64(attestation.AggregationBits.Count()) / inclusionDistance
		}
	}

	// Add slashing scores.
	// Slashing reward will be at most MAX_EFFECTIVE_BALANCE/WHISTLEBLOWER_REWARD_QUOTIENT,
	// which is 0.0625 Ether.
	// Individual attestation reward at 16K validators will be around 90,000 GWei, or .00009 Ether.
	// So we state that a single slashing event has the same weight as about 700 attestations.
	slashingWeight := float64(700)

	// Add proposer slashing scores.
	proposerSlashingScore := float64(len(blockProposal.Body.ProposerSlashings)) * slashingWeight

	// Add attester slashing scores.
	attesterSlashingScore := float64(len(blockProposal.Body.AttesterSlashings)) * slashingWeight

	log.Trace().
		Uint64("slot", slot).
		Str("provider", name).
		Float64("immediate_attestations", immediateAttestationScore).
		Float64("attestations", attestationScore).
		Float64("proposer_slashings", proposerSlashingScore).
		Float64("attester_slashings", attesterSlashingScore).
		Float64("total", attestationScore+proposerSlashingScore+attesterSlashingScore).
		Msg("Scored block")

	return attestationScore + proposerSlashingScore + attestationScore
}