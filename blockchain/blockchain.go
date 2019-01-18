package blockchain

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/shopspring/decimal"
	dbm "github.com/tendermint/tendermint/libs/db"
	"idena-go/blockchain/types"
	"idena-go/blockchain/validation"
	"idena-go/common"
	"idena-go/common/math"
	"idena-go/config"
	"idena-go/core/appstate"
	"idena-go/core/mempool"
	"idena-go/core/state"
	"idena-go/crypto"
	"idena-go/crypto/vrf"
	"idena-go/crypto/vrf/p256"
	"idena-go/log"
	"idena-go/rlp"
	"math/big"
	"time"
)

const (
	Mainnet types.Network = 0x1
	Testnet types.Network = 0x2
)

const (
	ProposerRole uint8 = 0x1
)

var (
	MaxHash *big.Float
)

type Blockchain struct {
	repo *repo

	Head            *types.Block
	genesis         *types.Block
	config          *config.Config
	vrfSigner       vrf.PrivateKey
	pubKey          *ecdsa.PublicKey
	coinBaseAddress common.Address
	log             log.Logger
	txpool          *mempool.TxPool
	appState        *appstate.AppState
}

func init() {
	var max [32]byte
	for i := range max {
		max[i] = 0xFF
	}
	i := new(big.Int)
	i.SetBytes(max[:])
	MaxHash = new(big.Float).SetInt(i)
}

func NewBlockchain(config *config.Config, db dbm.DB, txpool *mempool.TxPool, appState *appstate.AppState) *Blockchain {
	return &Blockchain{
		repo:     NewRepo(db),
		config:   config,
		log:      log.New(),
		txpool:   txpool,
		appState: appState,
	}
}

func (chain *Blockchain) GetHead() *types.Block {
	head := chain.repo.ReadHead()
	if head == nil {
		return nil
	}
	return chain.repo.ReadBlock(head.Hash())
}

func (chain *Blockchain) Network() types.Network {
	return chain.config.Network
}

func (chain *Blockchain) InitializeChain(secretKey *ecdsa.PrivateKey) error {
	signer, err := p256.NewVRFSigner(secretKey)
	if err != nil {
		return err
	}
	chain.vrfSigner = signer

	chain.pubKey = secretKey.Public().(*ecdsa.PublicKey)
	chain.coinBaseAddress = crypto.PubkeyToAddress(*chain.pubKey)
	head := chain.GetHead()
	if head != nil {
		chain.SetCurrentHead(head)
		if chain.genesis = chain.GetBlockByHeight(1); chain.genesis == nil {
			return errors.New("genesis block is not found")
		}
	} else {
		chain.GenerateGenesis(chain.config.Network)
	}
	log.Info("Chain initialized", "block", chain.Head.Hash().Hex(), "height", chain.Head.Height())
	return nil
}

func (chain *Blockchain) SetCurrentHead(block *types.Block) {
	chain.Head = block
}

func (chain *Blockchain) GenerateGenesis(network types.Network) *types.Block {
	chain.appState.State.SetNextEpochBlock(100)
	chain.appState.State.Commit(true)

	root := chain.appState.State.Root()

	var emptyHash [32]byte
	seed := types.Seed(crypto.Keccak256Hash(append([]byte{0x1, 0x2, 0x3, 0x4, 0x5, 0x6}, common.ToBytes(network)...)))
	block := &types.Block{Header: &types.Header{
		ProposedHeader: &types.ProposedHeader{
			ParentHash: emptyHash,
			Time:       big.NewInt(0),
			Height:     1,
			Root:       root,
		},
	}, Body: &types.Body{
		BlockSeed: seed,
	}}

	chain.insertBlock(block)
	chain.genesis = block
	return block
}

func (chain *Blockchain) GetBlockByHeight(height uint64) *types.Block {
	hash := chain.repo.ReadCanonicalHash(height)
	if hash == (common.Hash{}) {
		return nil
	}
	return chain.repo.ReadBlock(hash)
}

func (chain *Blockchain) GenerateEmptyBlock() *types.Block {
	head := chain.Head
	block := &types.Block{
		Header: &types.Header{
			EmptyBlockHeader: &types.EmptyBlockHeader{
				ParentHash: head.Hash(),
				Height:     head.Height() + 1,
				Root:       chain.appState.State.Root(),
			},
		},
		Body: &types.Body{
			Transactions: []*types.Transaction{},
		},
	}
	block.Body.BlockSeed = types.Seed(crypto.Keccak256Hash(chain.GetSeedData(block)))
	return block
}

func (chain *Blockchain) AddBlock(block *types.Block) error {

	if err := chain.validateBlockParentHash(block); err != nil {
		return err
	}
	if block.IsEmpty() {
		if err := chain.applyBlock(chain.appState.State, block); err != nil {
			return err
		}
		chain.insertBlock(chain.GenerateEmptyBlock())
	} else {
		if err := chain.ValidateProposedBlock(block); err != nil {
			return err
		}
		if err := chain.applyBlock(chain.appState.State, block); err != nil {
			return err
		}
		chain.insertBlock(block)
	}
	return nil
}

func (chain *Blockchain) applyBlock(state *state.StateDB, block *types.Block) error {
	if !block.IsEmpty() {
		if root, err := chain.applyAndValidateBlockState(state, block); err != nil {
			state.Reset()
			return err
		} else if root != block.Root() {
			state.Reset()
			return errors.New(fmt.Sprintf("Invalid block root. Exptected=%x, blockroot=%x", root, block.Root()))
		}
	}
	if block.Height() >= state.NextEpochBlock() {
		chain.applyNewEpoch(state)
	}

	hash, version, _ := state.Commit(true)
	chain.log.Trace("Applied block", "root", fmt.Sprintf("0x%x", hash), "version", version, "blockroot", block.Root())
	chain.txpool.ResetTo(block)
	chain.appState.ValidatorsCache.RefreshIfUpdated(block.Body.Transactions)
	return nil
}

func (chain *Blockchain) applyAndValidateBlockState(state *state.StateDB, block *types.Block) (common.Hash, error) {
	var totalFee *big.Int
	var err error
	if totalFee, err = chain.processTxs(state, block); err != nil {
		return common.Hash{}, err
	}
	return chain.applyBlockRewards(totalFee, state, block), nil
}

func (chain *Blockchain) applyBlockRewards(totalFee *big.Int, state *state.StateDB, block *types.Block) common.Hash {

	// calculate fee reward
	burnFee := decimal.NewFromBigInt(totalFee, 0)
	burnFee = burnFee.Mul(decimal.NewFromFloat32(chain.config.Consensus.FeeBurnRate))
	intBurn := math.ToInt(&burnFee)
	intFeeReward := new(big.Int)
	intFeeReward.Sub(totalFee, intBurn)

	// calculate stake
	stake := decimal.NewFromBigInt(chain.config.Consensus.BlockReward, 0)
	stake = stake.Mul(decimal.NewFromFloat32(chain.config.Consensus.StakeRewardRate))
	intStake := math.ToInt(&stake)

	// calculate block reward
	blockReward := big.NewInt(0)
	blockReward = blockReward.Sub(chain.config.Consensus.BlockReward, intStake)

	// calculate total reward
	totalReward := big.NewInt(0).Add(blockReward, intFeeReward)

	// update state
	state.AddBalance(block.Header.ProposedHeader.Coinbase, totalReward)
	state.AddStake(block.Header.ProposedHeader.Coinbase, intStake)
	state.AddInvite(block.Header.ProposedHeader.Coinbase, 1)

	chain.rewardFinalCommittee(state, block)
	state.Precommit(true)
	return state.Root()
}

func (chain *Blockchain) applyNewEpoch(stateDB *state.StateDB) {
	var verified []common.Address
	stateDB.IterateIdentities(func(key []byte, value []byte) bool {
		if key == nil {
			return true
		}
		addr := common.Address{}
		addr.SetBytes(key[1:])

		var data state.Identity
		if err := rlp.DecodeBytes(value, &data); err != nil {
			return false
		}
		if data.State == state.Candidate {
			verified = append(verified, addr)

		}
		return false
	})

	for _, addr := range verified {
		stateDB.GetOrNewIdentityObject(addr).SetState(state.Verified)
	}

	stateDB.IncEpoch()
	stateDB.SetNextEpochBlock(stateDB.NextEpochBlock() + 100)
}

func (chain *Blockchain) rewardFinalCommittee(state *state.StateDB, block *types.Block) {
	if block.IsEmpty() {
		return
	}
	identities := chain.appState.ValidatorsCache.GetActualValidators(chain.Head.Seed(), chain.Head.Height(), 1000, chain.GetCommitteSize(true))
	if identities == nil || identities.Cardinality() == 0 {
		return
	}
	totalReward := big.NewInt(0)
	totalReward.Div(chain.config.Consensus.FinalCommitteeReward, big.NewInt(int64(identities.Cardinality())))

	stake := decimal.NewFromBigInt(totalReward, 0)
	stake = stake.Mul(decimal.NewFromFloat32(chain.config.Consensus.StakeRewardRate))
	intStake := math.ToInt(&stake)

	reward := big.NewInt(0)
	reward.Sub(totalReward, intStake)

	for _, item := range identities.ToSlice() {
		addr := item.(common.Address)
		state.AddBalance(addr, reward)
		state.AddStake(addr, intStake)
	}
}

func (chain *Blockchain) processTxs(state *state.StateDB, block *types.Block) (*big.Int, error) {
	totalFee := new(big.Int)
	for i := 0; i < len(block.Body.Transactions); i++ {
		tx := block.Body.Transactions[i]
		if err := validation.ValidateTx(chain.appState, tx); err != nil {
			return nil, err
		}
		if fee, err := chain.applyTxOnState(state, tx); err != nil {
			return nil, err
		} else {
			totalFee.Add(totalFee, fee)
		}
	}

	return totalFee, nil
}

func (chain *Blockchain) applyTxOnState(stateDB *state.StateDB, tx *types.Transaction) (*big.Int, error) {
	sender, _ := types.Sender(tx)

	globalState := stateDB.GetOrNewGlobalObject()
	senderAccount := stateDB.GetOrNewAccountObject(sender)

	if tx.Epoch != globalState.Epoch() {
		return nil, errors.New(fmt.Sprintf("invalid tx epoch. Tx=%v expectedEpoch=%v actualEpoch=%v", tx.Hash().Hex(),
			globalState.Epoch(), tx.Epoch))
	}

	currentNonce := senderAccount.Nonce()
	// if epoch was increased, we should reset nonce to 1
	if senderAccount.Epoch() < globalState.Epoch() {
		currentNonce = 0
	}

	if currentNonce+1 != tx.AccountNonce {
		return nil, errors.New(fmt.Sprintf("invalid tx nonce. Tx=%v exptectedNonce=%v actualNonce=%v", tx.Hash().Hex(),
			currentNonce+1, tx.AccountNonce))
	}

	fee := chain.getTxFee(tx)
	totalCost := chain.getTxCost(tx)

	switch tx.Type {
	case types.ActivationTx:
		senderIdentity := stateDB.GetOrNewIdentityObject(sender)

		balance := stateDB.GetBalance(sender)
		change := new(big.Int).Sub(balance, totalCost)

		// zero balance and kill temp identity
		stateDB.SetBalance(sender, big.NewInt(0))
		senderIdentity.SetState(state.Killed)

		// verify identity and add transfer all available funds from temp account
		recipient := *tx.To
		stateDB.GetOrNewIdentityObject(recipient).SetState(state.Verified)
		stateDB.AddBalance(recipient, change)
		break
	case types.RegularTx:
		amount := tx.AmountOrZero()
		stateDB.SubBalance(sender, totalCost)
		stateDB.AddBalance(*tx.To, amount)
		break
	case types.InviteTx:

		stateDB.SubInvite(sender, 1)
		stateDB.SubBalance(sender, totalCost)

		stateDB.GetOrNewIdentityObject(*tx.To).SetState(state.Invite)
		stateDB.AddBalance(*tx.To, new(big.Int).Sub(totalCost, fee))
		break
	case types.KillTx:
		stateDB.GetOrNewIdentityObject(sender).SetState(state.Killed)
		break
	}

	stateDB.SetNonce(sender, tx.AccountNonce)

	if senderAccount.Epoch() != tx.Epoch {
		stateDB.SetEpoch(sender, tx.Epoch)
	}

	return fee, nil
}

func (chain *Blockchain) getTxFee(tx *types.Transaction) *big.Int {
	return types.CalculateFee(chain.appState.ValidatorsCache.GetCountOfValidNodes(), tx)
}

func (chain *Blockchain) getTxCost(tx *types.Transaction) *big.Int {
	return types.CalculateCost(chain.appState.ValidatorsCache.GetCountOfValidNodes(), tx)
}

func (chain *Blockchain) GetSeedData(proposalBlock *types.Block) []byte {
	head := chain.Head
	result := head.Seed().Bytes()
	result = append(result, common.ToBytes(proposalBlock.Height())...)
	result = append(result, proposalBlock.Hash().Bytes()...)
	return result
}

func (chain *Blockchain) GetProposerSortition() (bool, common.Hash, []byte) {
	return chain.getSortition(chain.getProposerData())
}

func (chain *Blockchain) ProposeBlock() *types.Block {
	head := chain.Head

	txs := chain.txpool.BuildBlockTransactions()
	checkState := state.NewForCheck(chain.appState.State, chain.Head.Height())
	filteredTxs, totalFee := chain.filterTxs(checkState, txs)

	header := &types.ProposedHeader{
		Height:         head.Height() + 1,
		ParentHash:     head.Hash(),
		Time:           new(big.Int).SetInt64(time.Now().UTC().Unix()),
		ProposerPubKey: crypto.FromECDSAPub(chain.pubKey),
		TxHash:         types.DeriveSha(types.Transactions(filteredTxs)),
		Coinbase:       chain.coinBaseAddress,
	}

	block := &types.Block{
		Header: &types.Header{
			ProposedHeader: header,
		},
		Body: &types.Body{
			Transactions: filteredTxs,
		},
	}
	block.Header.ProposedHeader.Root = chain.applyBlockRewards(totalFee, checkState, block)
	block.Body.BlockSeed, block.Body.SeedProof = chain.vrfSigner.Evaluate(chain.GetSeedData(block))

	return block
}

func (chain *Blockchain) filterTxs(state *state.StateDB, txs []*types.Transaction) ([]*types.Transaction, *big.Int) {
	var result []*types.Transaction

	totalFee := new(big.Int)
	for _, tx := range txs {
		if err := validation.ValidateTx(chain.appState, tx); err != nil {
			continue
		}
		if fee, err := chain.applyTxOnState(state, tx); err == nil {
			totalFee.Add(totalFee, fee)
			result = append(result, tx)
		}
	}
	return result, totalFee
}

func (chain *Blockchain) insertBlock(block *types.Block) {
	chain.repo.WriteBlock(block)
	chain.repo.WriteHead(block.Header)
	chain.repo.WriteCanonicalHash(block.Height(), block.Hash())
	chain.SetCurrentHead(block)
}

func (chain *Blockchain) getProposerData() []byte {
	head := chain.Head
	result := head.Seed().Bytes()
	result = append(result, common.ToBytes(ProposerRole)...)
	result = append(result, common.ToBytes(head.Height()+1)...)
	return result
}

func (chain *Blockchain) getSortition(data []byte) (bool, common.Hash, []byte) {
	hash, proof := chain.vrfSigner.Evaluate(data)

	v := new(big.Float).SetInt(new(big.Int).SetBytes(hash[:]))

	q := new(big.Float).Quo(v, MaxHash).SetPrec(10)

	if f, _ := q.Float64(); f >= chain.config.Consensus.ProposerTheshold {
		return true, hash, proof
	}
	return false, common.Hash{}, nil
}

func (chain *Blockchain) ValidateProposedBlock(block *types.Block) error {

	if err := chain.validateBlockParentHash(block); err != nil {
		return err
	}
	var seedData = chain.GetSeedData(block)
	pubKey, err := crypto.UnmarshalPubkey(block.Header.ProposedHeader.ProposerPubKey)
	if err != nil {
		return err
	}
	verifier, err := p256.NewVRFVerifier(pubKey)
	if err != nil {
		return err
	}

	hash, err := verifier.ProofToHash(seedData, block.Body.SeedProof)
	if err != nil {
		return err
	}
	if hash != block.Seed() || len(block.Seed()) == 0 {
		return errors.New("Seed is invalid")
	}

	proposerAddr, _ := crypto.PubKeyBytesToAddress(block.Header.ProposedHeader.ProposerPubKey)
	if chain.appState.ValidatorsCache.GetCountOfValidNodes() > 0 &&
		!chain.appState.ValidatorsCache.Contains(proposerAddr) {
		return errors.New("Proposer is not identity")
	}

	var txs = types.Transactions(block.Body.Transactions)

	if types.DeriveSha(txs) != block.Header.ProposedHeader.TxHash {
		return errors.New("TxHash is invalid")
	}

	for i := 0; i < len(block.Body.Transactions); i++ {
		tx := block.Body.Transactions[i]

		if err := validation.ValidateTx(chain.appState, tx); err != nil {
			return err
		}
	}
	checkState := state.NewForCheck(chain.appState.State, chain.Head.Height())
	if root, err := chain.applyAndValidateBlockState(checkState, block); err != nil {
		return err
	} else if root != block.Root() {
		return errors.New(fmt.Sprintf("Invalid block root. Exptected=%x, blockroot=%x", root, block.Root()))
	}
	return nil
}

func (chain *Blockchain) validateBlockParentHash(block *types.Block) error {
	head := chain.Head
	if head.Height()+1 != (block.Height()) {
		return errors.New(fmt.Sprintf("Height is invalid. Expected=%v but received=%v", head.Height()+1, block.Height()))
	}
	if head.Hash() != block.Header.ParentHash() {
		return errors.New("ParentHash is invalid")
	}
	return nil
}

func (chain *Blockchain) ValidateProposerProof(proof []byte, hash common.Hash, pubKeyData []byte) error {
	pubKey, err := crypto.UnmarshalPubkey(pubKeyData)
	if err != nil {
		return err
	}
	verifier, err := p256.NewVRFVerifier(pubKey)
	if err != nil {
		return err
	}

	h, err := verifier.ProofToHash(chain.getProposerData(), proof)

	if h != hash {
		return errors.New("Hashes are not equal")
	}

	v := new(big.Float).SetInt(new(big.Int).SetBytes(hash[:]))

	q := new(big.Float).Quo(v, MaxHash).SetPrec(10)

	if f, _ := q.Float64(); f < chain.config.Consensus.ProposerTheshold {
		return errors.New("Proposer is invalid")
	}

	proposerAddr := crypto.PubkeyToAddress(*pubKey)
	if chain.appState.ValidatorsCache.GetCountOfValidNodes() > 0 &&
		!chain.appState.ValidatorsCache.Contains(proposerAddr) {
		return errors.New("Proposer is not identity")
	}

	return nil
}

func (chain *Blockchain) Round() uint64 {
	return chain.Head.Height() + 1
}
func (chain *Blockchain) WriteFinalConsensus(hash common.Hash, cert *types.BlockCert) {
	chain.repo.WriteFinalConsensus(hash)
	chain.repo.WriteCert(hash, cert)
}
func (chain *Blockchain) GetBlock(hash common.Hash) *types.Block {
	return chain.repo.ReadBlock(hash)
}

func (chain *Blockchain) GetCommitteSize(final bool) int {
	var cnt = chain.appState.ValidatorsCache.GetCountOfValidNodes()
	percent := chain.config.Consensus.CommitteePercent
	if final {
		percent = chain.config.Consensus.FinalCommitteeConsensusPercent
	}
	if cnt <= 8 {
		return cnt
	}
	return int(float64(cnt) * percent)
}

func (chain *Blockchain) GetCommitteeVotesTreshold(final bool) int {

	var cnt = chain.appState.ValidatorsCache.GetCountOfValidNodes()
	percent := chain.config.Consensus.CommitteePercent
	if final {
		percent = chain.config.Consensus.FinalCommitteeConsensusPercent
	}

	switch cnt {
	case 1:
		return 1
	case 2, 3:
		return 2
	case 4, 5:
		return 3
	case 6, 7:
		return 4
	case 8:
		return 5
	}
	return int(float64(cnt) * percent * chain.config.Consensus.ThesholdBa)
}
func (chain *Blockchain) Genesis() common.Hash {
	return chain.genesis.Hash()
}
