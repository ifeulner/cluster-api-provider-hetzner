/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package host

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	infrav1 "github.com/syself/cluster-api-provider-hetzner/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/record"
)

// hostStateMachine is a finite state machine that manages transitions between
// the states of a BareMetalHost.
type hostStateMachine struct {
	host       *infrav1.HetznerBareMetalHost
	reconciler *Service
	nextState  infrav1.ProvisioningState
	log        *logr.Logger
}

func newHostStateMachine(host *infrav1.HetznerBareMetalHost, reconciler *Service, log *logr.Logger) *hostStateMachine {
	currentState := host.Spec.Status.ProvisioningState
	r := hostStateMachine{
		host:       host,
		reconciler: reconciler,
		nextState:  currentState, // Remain in current state by default
		log:        log,
	}
	return &r
}

type stateHandler func() actionResult

func (hsm *hostStateMachine) handlers() map[infrav1.ProvisioningState]stateHandler {
	return map[infrav1.ProvisioningState]stateHandler{
		infrav1.StatePreparing:         hsm.handlePreparing,
		infrav1.StateRegistering:       hsm.handleRegistering,
		infrav1.StateImageInstalling:   hsm.handleImageInstalling,
		infrav1.StateProvisioning:      hsm.handleProvisioning,
		infrav1.StateEnsureProvisioned: hsm.handleEnsureProvisioned,
		infrav1.StateProvisioned:       hsm.handleProvisioned,
		infrav1.StateDeprovisioning:    hsm.handleDeprovisioning,
		infrav1.StateDeleting:          hsm.handleDeleting,
	}
}

func (hsm *hostStateMachine) ReconcileState(ctx context.Context) (actionRes actionResult) {
	initialState := hsm.host.Spec.Status.ProvisioningState
	defer func() {
		if hsm.nextState != initialState {
			hsm.log.Info("changing provisioning state", "old", initialState, "new", hsm.nextState)
			hsm.host.Spec.Status.ProvisioningState = hsm.nextState
		}
	}()

	if hsm.checkInitiateDelete() {
		hsm.log.Info("Initiating host deletion")
		return actionComplete{}
	}

	actResult := hsm.updateSSHKey()
	if _, complete := actResult.(actionComplete); !complete {
		return actResult
	}

	if stateHandler, found := hsm.handlers()[initialState]; found {
		return stateHandler()
	}

	hsm.log.Info("No handler found for state", "state", initialState)
	return actionError{fmt.Errorf("no handler found for state \"%s\"", initialState)}
}

func (hsm *hostStateMachine) checkInitiateDelete() bool {
	if hsm.host.DeletionTimestamp.IsZero() {
		// Delete not requested
		return false
	}

	switch hsm.nextState {
	default:
		hsm.nextState = infrav1.StateDeleting
	case infrav1.StateRegistering, infrav1.StateImageInstalling, infrav1.StateProvisioning,
		infrav1.StateEnsureProvisioned, infrav1.StateProvisioned:
		hsm.nextState = infrav1.StateDeprovisioning
	case infrav1.StateDeprovisioning:
		// Continue deprovisioning.
		return false
	}
	return true
}

func (hsm *hostStateMachine) updateSSHKey() actionResult {
	// Skip if deprovisioning
	if hsm.host.Spec.Status.ProvisioningState == infrav1.StateDeprovisioning {
		return actionComplete{}
	}

	// Get ssh key secrets from secret
	osSSHSecret, rescueSSHSecret, err := hsm.reconciler.getSSHKeysAndUpdateStatus()
	if err != nil {
		return actionError{err: errors.Wrap(err, "failed to get ssh keys and update status")}
	}

	// Check whether os secret has been updated if it exists already
	if osSSHSecret != nil {
		if !hsm.host.Spec.Status.SSHStatus.CurrentOS.Match(*osSSHSecret) {
			// Take action depending on state
			switch hsm.nextState {
			case infrav1.StateProvisioning, infrav1.StateEnsureProvisioned:
				// Go back to StateImageInstalling as we need to provision again
				hsm.nextState = infrav1.StateImageInstalling
			case infrav1.StateProvisioned:
				errMessage := "secret has been modified although a provisioned machine uses it"
				record.Event(hsm.host, "SSHSecretUnexpectedlyModified", errMessage)
				return hsm.reconciler.recordActionFailure(infrav1.RegistrationError, errMessage)
			}
			if err := hsm.host.UpdateOSSSHStatus(*osSSHSecret); err != nil {
				return actionError{err: errors.Wrap(err, "failed to update status of OS SSH secret")}
			}
		}
		actResult := hsm.reconciler.validateSSHKey(osSSHSecret, "os")
		if _, complete := actResult.(actionComplete); !complete {
			return actResult
		}
	}

	if rescueSSHSecret != nil {
		if !hsm.host.Spec.Status.SSHStatus.CurrentRescue.Match(*rescueSSHSecret) {
			// Take action depending on state
			switch hsm.nextState {
			case infrav1.StatePreparing, infrav1.StateRegistering, infrav1.StateImageInstalling:
				hsm.log.Info("Attention: Going back to state none as rescue secret was updated", "state", hsm.nextState,
					"currentRescue", hsm.host.Spec.Status.SSHStatus.CurrentRescue)
				hsm.nextState = infrav1.StateNone
			}
			if err := hsm.host.UpdateRescueSSHStatus(*rescueSSHSecret); err != nil {
				return actionError{err: errors.Wrap(err, "failed to update status of rescue SSH secret")}
			}
		}
		actResult := hsm.reconciler.validateSSHKey(rescueSSHSecret, "rescue")
		if _, complete := actResult.(actionComplete); !complete {
			return actResult
		}
	}
	return actionComplete{}
}

func (hsm *hostStateMachine) handlePreparing() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}
	actResult := hsm.reconciler.actionPreparing()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateRegistering
	}
	return actResult
}

func (hsm *hostStateMachine) handleRegistering() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}

	actResult := hsm.reconciler.actionRegistering()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateImageInstalling
	}
	return actResult
}

func (hsm *hostStateMachine) handleImageInstalling() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}

	actResult := hsm.reconciler.actionImageInstalling()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateProvisioning
	}
	return actResult
}

func (hsm *hostStateMachine) handleProvisioning() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}

	actResult := hsm.reconciler.actionProvisioning()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateEnsureProvisioned
	}
	return actResult
}

func (hsm *hostStateMachine) handleEnsureProvisioned() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}

	actResult := hsm.reconciler.actionEnsureProvisioned()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateProvisioned
	}
	return actResult
}

func (hsm *hostStateMachine) handleProvisioned() actionResult {
	if hsm.provisioningCancelled() {
		hsm.nextState = infrav1.StateDeprovisioning
		return actionComplete{}
	}
	return hsm.reconciler.actionProvisioned()
}

func (hsm *hostStateMachine) handleDeprovisioning() actionResult {
	actResult := hsm.reconciler.actionDeprovisioning()
	if _, ok := actResult.(actionComplete); ok {
		hsm.nextState = infrav1.StateNone
		return actionComplete{}
	}
	return actResult
}

func (hsm *hostStateMachine) handleDeleting() actionResult {
	return hsm.reconciler.actionDeleting()
}

func (hsm *hostStateMachine) provisioningCancelled() bool {
	return hsm.host.Spec.Status.InstallImage == nil
}
