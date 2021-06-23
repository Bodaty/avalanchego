package proposervm

// VM is a decorator for a snowman.ChainVM struct, created to handle block headers introduced with snowman++

// Contract
// * CoreVM MUST build blocks on top of currently preferred block, otherwise Verify() will fail
// * After initialization. full ProposerBlocks (proHeader + core block) are stored in proposervm.VM's db
// on Build/ParseBlock calls, AFTER calls to core vm's Build/ParseBlock, which we ASSUME
//  would store core block on core VM's db.
// * ProposerVM do not track ProposerBlock state; instead state related calls (Accept/Reject/Status) are
// forwarded to the core VM. Since block registration HAPPENS BEFORE block status settings,
// proposerVM is guaranteed not to lose the last accepted block
// * ProposerVM can handle both ProposerVM blocks AND generic snowman.Block not wrapped with a ProposerBlocHeader
// This allows all snowman-like VM freedom to select a time after which introduce the congestion control mechanism
// implemented via the proposer block header

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"time"

	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils/hashing"
)

var (
	genesisParentID  = ids.Empty
	NoProposerBlocks = time.Unix(1<<63-62135596801, 999999999)
)

// clock interface and implementation, to ease up UTs
type clock interface {
	now() time.Time
}

type clockImpl struct{}

func (c clockImpl) now() time.Time {
	return time.Now()
}

type VM struct {
	block.ChainVM
	state.State
	proposer.Windower

	windower
	clock

	// node identity attributes
	stakingCert tls.Certificate
	nodeID      ids.ShortID

	scheduler *scheduler

	proBlkActivationTime time.Time
	BlkTree
}

func NewProVM(vm block.ChainVM, proBlkStart time.Time) *VM {
	res := VM{
		ChainVM:              vm,
		clock:                clockImpl{},
		proBlkActivationTime: proBlkStart,
	}
}

// common.VM interface implementation
func (vm *VM) Initialize(
	ctx *snow.Context,
	dbManager manager.Manager,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- common.Message,
	fxs []*common.Fx,
) error {
	vm.state.init(dbManager.Current().Database)

	var err error
	vm.stakingCert = ctx.StakingCert
	if vm.nodeID, err = ids.ToShortID(hashing.PubkeyBytesToAddress(vm.stakingCert.Leaf.Raw)); err != nil {
		return err
	}

	if err := vm.windower.initialize(vm, ctx); err != nil {
		return err
	}

	vm.scheduler = &scheduler{}
	if err := vm.scheduler.initialize(vm, toEngine); err != nil {
		return err
	}

	// TODO: comparison should be with genesis timestamp, not with Now()
	if vm.now().After(vm.proBlkActivationTime) {
		// proposerVM intercepts VM events for blocks and times event relay to consensus

		// Assuming genesisBytes has not proposerBlockHeader
		if err := vm.ChainVM.Initialize(ctx, dbManager, genesisBytes, upgradeBytes,
			configBytes, vm.scheduler.coreVMChannel(), fxs); err != nil {
			return err
		}

		_, err := vm.state.getProGenesisBlk()
		switch err {
		case ErrGenesisNotFound:
			// rebuild genesis and store it
			coreGenID, err := vm.ChainVM.LastAccepted()
			if err != nil {
				return err
			}
			coreGenBlk, err := vm.ChainVM.GetBlock(coreGenID)
			if err != nil {
				return err
			}
			proGenHdr := NewProHeader(genesisParentID, coreGenBlk.Timestamp().Unix(), 0, x509.Certificate{})
			proGenBlk, _ := NewProBlock(vm, proGenHdr, coreGenBlk, choices.Accepted, nil, false) // not signing block, cannot err
			// Skipping verification for genesis block.
			if err := vm.state.storeProGenID(proGenBlk.ID()); err != nil {
				return err
			}
			if err := vm.state.storePreference(proGenBlk.ID()); err != nil {
				return err
			}
			if err := vm.state.storeLastAcceptedID(proGenBlk.ID()); err != nil {
				return err
			}
			if err := vm.state.storeProBlk(&proGenBlk); err != nil {
				return err
			}

			vm.BlkTree.Initialize(vm, proGenBlk.ID())
		case nil: // TODO: do checks on Preference and LastAcceptedID or just keep going?
		default:
			return err
		}

		vm.scheduler.rescheduleBlkTicker()
		go vm.scheduler.handleBlockTiming()
	} else if err := vm.ChainVM.Initialize(ctx, dbManager, genesisBytes, upgradeBytes,
		configBytes, toEngine, fxs); err != nil {
		return err
	}

	return nil
}

// block.ChainVM interface implementation
func (vm *VM) BuildBlock() (snowman.Block, error) {
	sb, err := vm.ChainVM.BuildBlock()
	if err != nil {
		return nil, err
	}

	// TODO: comparison should be with genesis timestamp, not with Now()
	if vm.now().After(vm.proBlkActivationTime) {
		proParentID, err := vm.state.getPreferredID()
		if err != nil {
			return nil, err
		}

	proParentID, err := vm.state.getPreferredID()
	if err != nil {
		return nil, err
	}

	h, err := vm.pChainHeight()
	if err != nil {
		return nil, err
	}

	slb, err := statelessblock.Build(
		proParentID,
		sb.Timestamp(),
		h,
		vm.stakingCert.Leaf,
		sb.Bytes(),
		vm.stakingCert.PrivateKey.(crypto.Signer),
	)
	if err != nil {
		return nil, err
	}

	proBlk := ProposerBlock{
		Block:   slb,
		vm:      vm,
		coreBlk: sb,
		status:  choices.Processing,
	}

	if err := proBlk.Verify(); err != nil {
		return nil, err
	}

	if err := vm.state.storeProBlk(&proBlk); err != nil {
		return nil, err
	}

	return &proBlk, nil
}

func (vm *VM) ParseBlock(b []byte) (snowman.Block, error) {
	block, err := vm.parseProposerBlock(b)
	if err == nil {
		return &block, nil
	}
	return vm.ChainVM.ParseBlock(b)
}

func (vm *VM) parseProposerBlock(b []byte) (ProposerBlock, error) {
	slb, err := statelessblock.Parse(b)
	if err != nil {
		return ProposerBlock{}, err
	}

	coreBlk, err := vm.ChainVM.ParseBlock(slb.Block())
	if err != nil {
		return ProposerBlock{}, err
	}

	block := ProposerBlock{
		Block:   slb,
		vm:      vm,
		coreBlk: coreBlk,
		status:  choices.Processing,
	}

	if err := vm.state.storeProBlk(&block); err != nil {
		return ProposerBlock{}, err
	}

	return block, nil
}

func (vm *VM) GetBlock(id ids.ID) (snowman.Block, error) {
	if res, err := vm.state.getProBlock(id); err == nil {
		return res, nil
	}

	// check whether block is core one, with no proposerBlockHeader
	if coreBlk, err := vm.ChainVM.GetBlock(id); err == nil {
		return coreBlk, nil
	}

	return nil, ErrProBlkNotFound
}

func (vm *VM) SetPreference(id ids.ID) error {
	currPrefID, err := vm.state.getPreferredID()
	switch err {
	case nil:
		proBlk, err := vm.state.getProBlock(id)
		if err != nil {
			return err
		}
		if err := vm.state.storePreference(id); err != nil {
			return err
		}
		if err := vm.ChainVM.SetPreference(proBlk.coreBlk.ID()); err != nil {
			// attempt restoring previous proposer block reference and return error
			if err := vm.state.storePreference(currPrefID); err != nil {
				// TODO log
				return err
			}
			return err
		}
		return nil
	case ErrPreferredIDNotFound: // pre snowman++ case
		return vm.ChainVM.SetPreference(id)
	default:
		return err
	}
}

func (vm *VM) LastAccepted() (ids.ID, error) {
	res, err := vm.state.getLastAcceptedID()
	switch err {
	case nil:
		return res, nil
	case ErrLastAcceptedIDNotFound: // pre snowman++ case
		return vm.ChainVM.LastAccepted()
	default:
		return res, err
	}
}