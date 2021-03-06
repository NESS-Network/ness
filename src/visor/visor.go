/*
Package visor manages the blockchain database and wallets

All conceptual database operations must use a database transaction.
Callers of visor methods must ensure they do not make multiple calls without a transaction,
unless it is determined safe to do so.

Wallet access is also gatewayed by visor, since the wallet data relates to the blockchain database.
Wallets are conceptually a second database.
*/
package visor

import (
	"errors"
	"fmt"

	"time"

	"github.com/skycoin/skycoin/src/cipher"
	"github.com/skycoin/skycoin/src/coin"
	"github.com/skycoin/skycoin/src/params"
	"github.com/skycoin/skycoin/src/transaction"
	"github.com/skycoin/skycoin/src/util/logging"
	"github.com/skycoin/skycoin/src/util/mathutil"
	"github.com/skycoin/skycoin/src/util/timeutil"
	"github.com/skycoin/skycoin/src/visor/blockdb"
	"github.com/skycoin/skycoin/src/visor/dbutil"
	"github.com/skycoin/skycoin/src/visor/historydb"
	"github.com/skycoin/skycoin/src/wallet"
)

var logger = logging.MustGetLogger("visor")

// Visor manages the blockchain
type Visor struct {
	Config Config

	startedAt   time.Time
	db          *dbutil.DB
	unconfirmed UnconfirmedTransactionPooler
	blockchain  Blockchainer
	history     Historyer
	wallets     *wallet.Service
	txns        transactionsGetter
	tf          wallet.TransactionsFinder
}

// New creates a Visor for managing the blockchain database
func New(c Config, db *dbutil.DB, wltServ *wallet.Service) (*Visor, error) {
	logger.Info("Creating new visor")
	if c.IsBlockPublisher {
		logger.Info("Visor running in block publisher mode")
	}

	if err := c.Verify(); err != nil {
		return nil, err
	}

	logger.Infof("Coinhour burn factor for unconfirmed transactions is %d", c.UnconfirmedVerifyTxn.BurnFactor)
	logger.Infof("Max transaction size for unconfirmed transactions is %d", c.UnconfirmedVerifyTxn.MaxTransactionSize)
	logger.Infof("Max decimals for unconfirmed transactions is %d", c.UnconfirmedVerifyTxn.MaxDropletPrecision)
	logger.Infof("Coinhour burn factor for transactions when creating blocks is %d", c.CreateBlockVerifyTxn.BurnFactor)
	logger.Infof("Max transaction size for transactions when creating blocks is %d", c.CreateBlockVerifyTxn.MaxTransactionSize)
	logger.Infof("Max decimals for transactions when creating blocks is %d", c.CreateBlockVerifyTxn.MaxDropletPrecision)
	logger.Infof("Max block size is %d", c.MaxBlockTransactionsSize)

	if !db.IsReadOnly() {
		if err := CreateBuckets(db); err != nil {
			logger.WithError(err).Error("CreateBuckets failed")
			return nil, err
		}
	}

	bc, err := NewBlockchain(db, BlockchainConfig{
		Pubkey:      c.BlockchainPubkey,
		Arbitrating: c.Arbitrating,
	})
	if err != nil {
		return nil, err
	}

	history := historydb.New()

	if !db.IsReadOnly() {
		if err := db.Update("build unspent indexes and init history", func(tx *dbutil.Tx) error {
			headSeq, _, err := bc.HeadSeq(tx)
			if err != nil {
				return err
			}

			if err := bc.Unspent().MaybeBuildIndexes(tx, headSeq); err != nil {
				return err
			}

			return initHistory(tx, bc, history)
		}); err != nil {
			return nil, err
		}
	}

	utp, err := NewUnconfirmedTransactionPool(db)
	if err != nil {
		return nil, err
	}

	txns := transactionModel{
		history:     history,
		unconfirmed: utp,
		blockchain:  bc,
	}

	v := &Visor{
		Config:      c,
		startedAt:   time.Now(),
		db:          db,
		blockchain:  bc,
		unconfirmed: utp,
		history:     history,
		wallets:     wltServ,
		txns:        &txns,
	}

	v.tf = newTransactionsFinder(v)

	return v, nil
}

// VisorConfig returns Config
func (vs *Visor) VisorConfig() Config {
	return vs.Config
}

// Init initializes starts the visor
func (vs *Visor) Init() error {
	logger.Info("Visor init")

	if vs.db.IsReadOnly() {
		return nil
	}

	return vs.db.Update("visor init", func(tx *dbutil.Tx) error {
		if err := vs.maybeCreateGenesisBlock(tx); err != nil {
			return err
		}

		removed, err := vs.unconfirmed.RemoveInvalid(tx, vs.blockchain)
		if err != nil {
			return err
		}
		logger.Infof("Removed %d invalid txns from pool", len(removed))

		return nil
	})
}

func initHistory(tx *dbutil.Tx, bc *Blockchain, history *historydb.HistoryDB) error {
	logger.Info("Visor initHistory")

	shouldReset, err := history.NeedsReset(tx)
	if err != nil {
		return err
	}

	if !shouldReset {
		return nil
	}

	logger.Info("Resetting historyDB")

	if err := history.Erase(tx); err != nil {
		return err
	}

	// Reparse the history up to the blockchain head
	headSeq, _, err := bc.HeadSeq(tx)
	if err != nil {
		return err
	}

	if err := parseHistoryTo(tx, history, bc, headSeq); err != nil {
		logger.WithError(err).Error("parseHistoryTo failed")
		return err
	}

	return nil
}

func parseHistoryTo(tx *dbutil.Tx, history *historydb.HistoryDB, bc *Blockchain, height uint64) error {
	logger.Info("Visor parseHistoryTo")

	parsedBlockSeq, _, err := history.ParsedBlockSeq(tx)
	if err != nil {
		return err
	}

	for i := uint64(0); i < height-parsedBlockSeq; i++ {
		b, err := bc.GetSignedBlockBySeq(tx, parsedBlockSeq+i+1)
		if err != nil {
			return err
		}

		if b == nil {
			return fmt.Errorf("no block exists in depth: %d", parsedBlockSeq+i+1)
		}

		if err := history.ParseBlock(tx, b.Block); err != nil {
			return err
		}
	}

	return nil
}

// maybeCreateGenesisBlock creates a genesis block if necessary
func (vs *Visor) maybeCreateGenesisBlock(tx *dbutil.Tx) error {
	logger.Info("Visor maybeCreateGenesisBlock")
	gb, err := vs.blockchain.GetGenesisBlock(tx)
	if err != nil {
		return err
	}
	if gb != nil {
		return nil
	}

	logger.Info("Create genesis block")
	vs.GenesisPreconditions()
	b, err := coin.NewGenesisBlock(vs.Config.GenesisAddress, vs.Config.GenesisCoinVolume, vs.Config.GenesisTimestamp)
	if err != nil {
		return err
	}

	var sb coin.SignedBlock
	// record the signature of genesis block
	if vs.Config.IsBlockPublisher {
		sb = vs.signBlock(*b)
		logger.Infof("Genesis block signature=%s", sb.Sig.Hex())
	} else {
		sb = coin.SignedBlock{
			Block: *b,
			Sig:   vs.Config.GenesisSignature,
		}
	}

	return vs.executeSignedBlock(tx, sb)
}

// GenesisPreconditions panics if conditions for genesis block are not met
func (vs *Visor) GenesisPreconditions() {
	if vs.Config.BlockchainSeckey != (cipher.SecKey{}) {
		if vs.Config.BlockchainPubkey != cipher.MustPubKeyFromSecKey(vs.Config.BlockchainSeckey) {
			logger.Panic("Cannot create genesis block. Invalid secret key for pubkey")
		}
	}
}

// StartedAt returns the time that the visor was created
func (vs *Visor) StartedAt() time.Time {
	return vs.startedAt
}

// RefreshUnconfirmed checks unconfirmed txns against the blockchain and returns
// all transaction that turn to valid.
func (vs *Visor) RefreshUnconfirmed() ([]cipher.SHA256, error) {
	var hashes []cipher.SHA256
	if err := vs.db.Update("RefreshUnconfirmed", func(tx *dbutil.Tx) error {
		var err error
		hashes, err = vs.unconfirmed.Refresh(tx, vs.blockchain, vs.Config.Distribution, vs.Config.UnconfirmedVerifyTxn)
		return err
	}); err != nil {
		return nil, err
	}

	return hashes, nil
}

// RemoveInvalidUnconfirmed removes transactions that become permanently invalid
// (by violating hard constraints) from the pool.
// Returns the transaction hashes that were removed.
func (vs *Visor) RemoveInvalidUnconfirmed() ([]cipher.SHA256, error) {
	var hashes []cipher.SHA256
	if err := vs.db.Update("RemoveInvalidUnconfirmed", func(tx *dbutil.Tx) error {
		var err error
		hashes, err = vs.unconfirmed.RemoveInvalid(tx, vs.blockchain)
		return err
	}); err != nil {
		return nil, err
	}

	return hashes, nil
}

// createBlock creates a SignedBlock from pending transactions
func (vs *Visor) createBlock(tx *dbutil.Tx, when uint64) (coin.SignedBlock, error) {
	if !vs.Config.IsBlockPublisher {
		logger.Panic("Only a block publisher node can create blocks")
	}

	// Gather all unconfirmed transactions
	txns, err := vs.unconfirmed.AllRawTransactions(tx)
	if err != nil {
		return coin.SignedBlock{}, err
	}

	b, err := vs.createBlockFromTxns(tx, txns, when)
	if err != nil {
		return coin.SignedBlock{}, err
	}

	return vs.signBlock(b), nil
}

// createBlockFromTxns creates a Block from specified set of transactions according to set of determinstic rules.
func (vs *Visor) createBlockFromTxns(tx *dbutil.Tx, txns coin.Transactions, when uint64) (coin.Block, error) {
	if len(txns) == 0 {
		return coin.Block{}, errors.New("No transactions")
	}

	logger.Infof("unconfirmed pool has %d transactions pending", len(txns))

	// Filter transactions that violate all constraints
	var filteredTxns coin.Transactions
	for _, txn := range txns {
		if _, _, err := vs.blockchain.VerifySingleTxnSoftHardConstraints(tx, txn, vs.Config.Distribution, vs.Config.CreateBlockVerifyTxn, transaction.TxnSigned); err != nil {
			switch err.(type) {
			case transaction.ErrTxnViolatesHardConstraint, transaction.ErrTxnViolatesSoftConstraint:
				logger.Warningf("Transaction %s violates constraints: %v", txn.Hash().Hex(), err)
			default:
				return coin.Block{}, err
			}
		} else {
			filteredTxns = append(filteredTxns, txn)
		}
	}

	nRemoved := len(txns) - len(filteredTxns)
	if nRemoved > 0 {
		logger.Infof("CreateBlock ignored %d transactions violating constraints", nRemoved)
	}

	txns = filteredTxns

	if len(txns) == 0 {
		logger.Info("No transactions after filtering for constraint violations")
		return coin.Block{}, errors.New("No transactions after filtering for constraint violations")
	}

	head, err := vs.blockchain.Head(tx)
	if err != nil {
		return coin.Block{}, err
	}

	// Sort them by highest fee per kilobyte
	txns, err = coin.SortTransactions(txns, vs.blockchain.TransactionFee(tx, head.Time()))
	if err != nil {
		logger.Critical().WithError(err).Error("SortTransactions failed, no block can be made until the offending transaction is removed")
		return coin.Block{}, err
	}

	// Apply block size transaction limit
	txns, err = txns.TruncateBytesTo(vs.Config.MaxBlockTransactionsSize)
	if err != nil {
		logger.Critical().WithError(err).Error("TruncateBytesTo failed, no block can be made until the offending transaction is removed")
		return coin.Block{}, err
	}

	if len(txns) > coin.MaxBlockTransactions {
		txns = txns[:coin.MaxBlockTransactions]
	}

	if len(txns) == 0 {
		logger.Panic("TruncateBytesTo removed all transactions")
	}

	logger.Infof("Creating new block with %d transactions, head time %d", len(txns), when)

	b, err := vs.blockchain.NewBlock(tx, txns, when)
	if err != nil {
		logger.Warningf("blockchain.NewBlock failed: %v", err)
		return coin.Block{}, err
	}

	return *b, nil
}

// CreateAndExecuteBlock creates a SignedBlock from pending transactions and executes it
func (vs *Visor) CreateAndExecuteBlock() (coin.SignedBlock, error) {
	var sb coin.SignedBlock

	err := vs.db.Update("CreateAndExecuteBlock", func(tx *dbutil.Tx) error {
		var err error
		sb, err = vs.createBlock(tx, uint64(time.Now().UTC().Unix()))
		if err != nil {
			return err
		}

		return vs.executeSignedBlock(tx, sb)
	})

	return sb, err
}

// CreateBlockFromTxns creates a Block from specified set of transactions according to set of determinstic rules.
func (vs *Visor) CreateBlockFromTxns(txns coin.Transactions, when uint64) (coin.Block, error) {
	var sb coin.Block

	err := vs.db.Update("CreateBlockFromTxns", func(tx *dbutil.Tx) error {
		var err error
		if sb, err = vs.createBlockFromTxns(tx, txns, when); err != nil {
			return err
		}

		return nil
	})

	return sb, err
}

// VerifyBlock verifies specified block against local copy of blockchain.
// Signature is not verified.
func (vs *Visor) VerifyBlock(b coin.SignedBlock) error {
	return vs.db.View("VerifyBlock", func(tx *dbutil.Tx) error {
		return vs.blockchain.VerifyBlock(tx, &b)
	})
}

// ExecuteSignedBlock adds a block to the blockchain, or returns error.
// Blocks must be executed in sequence, and be signed by a block publisher node.
func (vs *Visor) ExecuteSignedBlock(b coin.SignedBlock) error {
	return vs.db.Update("ExecuteSignedBlock", func(tx *dbutil.Tx) error {
		return vs.executeSignedBlock(tx, b)
	})
}

// ExecuteSignedBlockUnsafe adds block to the blockchain, or returns error.
// Blocks must be executed in sequence. Block signature is not verified.
func (vs *Visor) ExecuteSignedBlockUnsafe(b coin.SignedBlock) error {
	return vs.db.Update("ExecuteSignedBlockUnsafe", func(tx *dbutil.Tx) error {
		return vs.executeSignedBlockUnsafe(tx, b)
	})
}

// executeSignedBlock adds a block to the blockchain, or returns error.
// Blocks must be executed in sequence, and be signed by a block publisher node.
func (vs *Visor) executeSignedBlock(tx *dbutil.Tx, b coin.SignedBlock) error {
	if err := b.VerifySignature(vs.Config.BlockchainPubkey); err != nil {
		return err
	}

	return vs.executeSignedBlockUnsafe(tx, b)
}

// executeSignedBlockUnsafe add a block to the blockchain, or returns error.
// Blocks must be executed in sequence. Block signature is not verified.
func (vs *Visor) executeSignedBlockUnsafe(tx *dbutil.Tx, b coin.SignedBlock) error {
	if err := vs.blockchain.ExecuteBlock(tx, &b); err != nil {
		return err
	}

	// Remove the transactions in the Block from the unconfirmed pool
	txnHashes := make([]cipher.SHA256, 0, len(b.Block.Body.Transactions))
	for _, txn := range b.Block.Body.Transactions {
		txnHashes = append(txnHashes, txn.Hash())
	}

	if err := vs.unconfirmed.RemoveTransactions(tx, txnHashes); err != nil {
		return err
	}

	// Update the HistoryDB
	return vs.history.ParseBlock(tx, b.Block)
}

// signBlock signs a block for a block publisher node. Will panic if anything is invalid
func (vs *Visor) signBlock(b coin.Block) coin.SignedBlock {
	if !vs.Config.IsBlockPublisher {
		logger.Panic("Only a block publisher node can sign blocks")
	}

	sig := cipher.MustSignHash(b.HashHeader(), vs.Config.BlockchainSeckey)

	return coin.SignedBlock{
		Block: b,
		Sig:   sig,
	}
}

/*
	Return Data
*/

// GetAllUnspentOutputs returns all unspent outputs
func (vs *Visor) GetAllUnspentOutputs() (coin.UxArray, error) {
	var ux []coin.UxOut
	if err := vs.db.View("GetAllUnspentOutputs", func(tx *dbutil.Tx) error {
		var err error
		ux, err = vs.blockchain.Unspent().GetAll(tx)
		return err
	}); err != nil {
		return nil, err
	}

	return ux, nil
}

// GetUnspentOutputs returns unspent outputs from the pool, queried by hashes.
// If any do not exist, ErrUnspentNotExist is returned
func (vs *Visor) GetUnspentOutputs(hashes []cipher.SHA256) (coin.UxArray, error) {
	var outputs coin.UxArray
	if err := vs.db.View("GetUnspentOutputs", func(tx *dbutil.Tx) error {
		var err error
		outputs, err = vs.blockchain.Unspent().GetArray(tx, hashes)
		return err
	}); err != nil {
		return nil, err
	}

	return outputs, nil
}

// UnconfirmedOutgoingOutputs returns all outputs that would be spent by unconfirmed transactions
func (vs *Visor) UnconfirmedOutgoingOutputs() (coin.UxArray, error) {
	var uxa coin.UxArray

	if err := vs.db.View("UnconfirmedOutgoingOutputs", func(tx *dbutil.Tx) error {
		var err error
		uxa, err = vs.unconfirmedOutgoingOutputs(tx)
		return err
	}); err != nil {
		return nil, err
	}

	return uxa, nil
}

func (vs *Visor) unconfirmedOutgoingOutputs(tx *dbutil.Tx) (coin.UxArray, error) {
	txns, err := vs.unconfirmed.AllRawTransactions(tx)
	if err != nil {
		return nil, err
	}

	var inputs []cipher.SHA256
	for _, txn := range txns {
		inputs = append(inputs, txn.In...)
	}

	return vs.blockchain.Unspent().GetArray(tx, inputs)
}

// UnconfirmedIncomingOutputs returns all outputs that would be created by unconfirmed transactions
func (vs *Visor) UnconfirmedIncomingOutputs() (coin.UxArray, error) {
	var uxa coin.UxArray

	if err := vs.db.View("UnconfirmedIncomingOutputs", func(tx *dbutil.Tx) error {
		var err error
		uxa, err = vs.unconfirmedIncomingOutputs(tx)
		return err
	}); err != nil {
		return nil, err
	}

	return uxa, nil
}

func (vs *Visor) unconfirmedIncomingOutputs(tx *dbutil.Tx) (coin.UxArray, error) {
	head, err := vs.blockchain.Head(tx)
	if err != nil {
		return nil, err
	}

	return vs.unconfirmed.GetIncomingOutputs(tx, head.Head)
}

// GetSignedBlocksSince returns N signed blocks more recent than Seq. Does not return nil.
func (vs *Visor) GetSignedBlocksSince(seq, ct uint64) ([]coin.SignedBlock, error) {
	var blocks []coin.SignedBlock

	if err := vs.db.View("GetSignedBlocksSince", func(tx *dbutil.Tx) error {
		avail := uint64(0)
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		headSeq := head.Seq()
		if headSeq > seq {
			avail = headSeq - seq
		}
		if avail < ct {
			ct = avail
		}
		if ct == 0 {
			return nil
		}

		blocks = make([]coin.SignedBlock, 0, ct)
		for j := uint64(0); j < ct; j++ {
			i := seq + 1 + j
			b, err := vs.blockchain.GetSignedBlockBySeq(tx, i)
			if err != nil {
				return err
			}

			blocks = append(blocks, *b)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return blocks, nil
}

// HeadBkSeq returns the highest BkSeq we know, returns false in the 2nd return value
// if the blockchain is empty
func (vs *Visor) HeadBkSeq() (uint64, bool, error) {
	var headSeq uint64
	var ok bool

	if err := vs.db.View("HeadBkSeq", func(tx *dbutil.Tx) error {
		var err error
		headSeq, ok, err = vs.blockchain.HeadSeq(tx)
		return err
	}); err != nil {
		return 0, false, err
	}

	return headSeq, ok, nil
}

// GetBlockchainMetadata returns descriptive blockchain information
func (vs *Visor) GetBlockchainMetadata() (*BlockchainMetadata, error) {
	var head *coin.SignedBlock
	var unconfirmedLen, unspentsLen uint64

	if err := vs.db.View("GetBlockchainMetadata", func(tx *dbutil.Tx) error {
		var err error
		head, err = vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		unconfirmedLen, err = vs.unconfirmed.Len(tx)
		if err != nil {
			return err
		}

		unspentsLen, err = vs.blockchain.Unspent().Len(tx)
		return err
	}); err != nil {
		return nil, err
	}

	return NewBlockchainMetadata(*head, unconfirmedLen, unspentsLen)
}

// GetBlock returns a copy of the block at seq. Returns error if seq out of range
func (vs *Visor) GetBlock(seq uint64) (*coin.SignedBlock, error) {
	var b *coin.SignedBlock

	if err := vs.db.View("GetBlock", func(tx *dbutil.Tx) error {
		headSeq, ok, err := vs.blockchain.HeadSeq(tx)
		if err != nil {
			return err
		}

		if !ok || seq > headSeq {
			return errors.New("Block seq out of range")
		}

		b, err = vs.blockchain.GetSignedBlockBySeq(tx, seq)
		return err
	}); err != nil {
		return nil, err
	}

	return b, nil
}

// GetBlocks returns blocks matches seqs
func (vs *Visor) GetBlocks(seqs []uint64) ([]coin.SignedBlock, error) {
	var blocks []coin.SignedBlock

	if err := vs.db.View("GetBlocks", func(tx *dbutil.Tx) error {
		var err error
		blocks, err = vs.blockchain.GetBlocks(tx, seqs)
		return err
	}); err != nil {
		return nil, err
	}

	return blocks, nil
}

// GetBlocksVerbose returns blocks matches seqs along with verbose transaction input data
func (vs *Visor) GetBlocksVerbose(seqs []uint64) ([]coin.SignedBlock, [][][]TransactionInput, error) {
	var blocks []coin.SignedBlock
	var inputs [][][]TransactionInput

	if err := vs.db.View("GetBlocksVerbose", func(tx *dbutil.Tx) error {
		var err error
		blocks, inputs, err = vs.getBlocksVerbose(tx, func(tx *dbutil.Tx) ([]coin.SignedBlock, error) {
			return vs.blockchain.GetBlocks(tx, seqs)
		})
		return err
	}); err != nil {
		return nil, nil, err
	}

	return blocks, inputs, nil
}

// GetBlocksInRange returns multiple blocks between start and end, including both start and end.
// Returns the empty slice if unable to fulfill request.
func (vs *Visor) GetBlocksInRange(start, end uint64) ([]coin.SignedBlock, error) {
	var blocks []coin.SignedBlock

	if err := vs.db.View("GetBlocksInRange", func(tx *dbutil.Tx) error {
		var err error
		blocks, err = vs.blockchain.GetBlocksInRange(tx, start, end)
		return err
	}); err != nil {
		return nil, err
	}

	return blocks, nil
}

// GetBlocksInRangeVerbose returns multiple blocks between start and end, including both start and end.
// Also returns the verbose transaction input data for transactions in these blocks.
// Returns the empty slice if unable to fulfill request.
func (vs *Visor) GetBlocksInRangeVerbose(start, end uint64) ([]coin.SignedBlock, [][][]TransactionInput, error) {
	var blocks []coin.SignedBlock
	var inputs [][][]TransactionInput

	if err := vs.db.View("GetBlocksInRangeVerbose", func(tx *dbutil.Tx) error {
		var err error
		blocks, inputs, err = vs.getBlocksVerbose(tx, func(tx *dbutil.Tx) ([]coin.SignedBlock, error) {
			return vs.blockchain.GetBlocksInRange(tx, start, end)
		})
		return err
	}); err != nil {
		return nil, nil, err
	}

	return blocks, inputs, nil
}

// GetLastBlocks returns last N blocks
func (vs *Visor) GetLastBlocks(num uint64) ([]coin.SignedBlock, error) {
	var blocks []coin.SignedBlock

	if err := vs.db.View("GetLastBlocks", func(tx *dbutil.Tx) error {
		var err error
		blocks, err = vs.blockchain.GetLastBlocks(tx, num)
		return err
	}); err != nil {
		return nil, err
	}

	return blocks, nil
}

// GetLastBlocksVerbose returns last N blocks with verbose transaction input data
func (vs *Visor) GetLastBlocksVerbose(num uint64) ([]coin.SignedBlock, [][][]TransactionInput, error) {
	var blocks []coin.SignedBlock
	var inputs [][][]TransactionInput

	if err := vs.db.View("GetLastBlocksVerbose", func(tx *dbutil.Tx) error {
		var err error
		blocks, inputs, err = vs.getBlocksVerbose(tx, func(tx *dbutil.Tx) ([]coin.SignedBlock, error) {
			return vs.blockchain.GetLastBlocks(tx, num)
		})
		return err
	}); err != nil {
		return nil, nil, err
	}

	return blocks, inputs, nil
}

func (vs *Visor) getBlocksVerbose(tx *dbutil.Tx, getBlocks func(*dbutil.Tx) ([]coin.SignedBlock, error)) ([]coin.SignedBlock, [][][]TransactionInput, error) {
	blocks, err := getBlocks(tx)
	if err != nil {
		return nil, nil, err
	}

	if len(blocks) == 0 {
		return nil, nil, nil
	}

	inputs := make([][][]TransactionInput, len(blocks))
	for i, b := range blocks {
		blockInputs, err := vs.getBlockInputs(tx, &b)
		if err != nil {
			return nil, nil, err
		}
		inputs[i] = blockInputs
	}

	return blocks, inputs, nil
}

// InjectForeignTransaction records a coin.Transaction to the UnconfirmedTransactionPool if the txn is not
// already in the blockchain.
// The bool return value is whether or not the transaction was already in the pool.
// If the transaction violates hard constraints, it is rejected, and error will not be nil.
// If the transaction only violates soft constraints, it is still injected, and the soft constraint violation is returned.
// This method is intended for transactions received over the network.
func (vs *Visor) InjectForeignTransaction(txn coin.Transaction) (bool, *transaction.ErrTxnViolatesSoftConstraint, error) {
	var known bool
	var softErr *transaction.ErrTxnViolatesSoftConstraint

	if err := vs.db.Update("InjectForeignTransaction", func(tx *dbutil.Tx) error {
		var err error
		known, softErr, err = vs.unconfirmed.InjectTransaction(tx, vs.blockchain, txn, vs.Config.Distribution, vs.Config.UnconfirmedVerifyTxn)
		return err
	}); err != nil {
		return false, nil, err
	}

	return known, softErr, nil
}

// InjectUserTransaction records a coin.Transaction to the UnconfirmedTransactionPool if the txn is not
// already in the blockchain.
// The bool return value is whether or not the transaction was already in the pool.
// If the transaction violates hard or soft constraints, it is rejected, and error will not be nil.
func (vs *Visor) InjectUserTransaction(txn coin.Transaction) (bool, *coin.SignedBlock, coin.UxArray, error) {
	var known bool
	var head *coin.SignedBlock
	var inputs coin.UxArray

	if err := vs.db.Update("InjectUserTransaction", func(tx *dbutil.Tx) error {
		var err error
		known, head, inputs, err = vs.InjectUserTransactionTx(tx, txn)
		return err
	}); err != nil {
		return false, nil, nil, err
	}

	return known, head, inputs, nil
}

// InjectUserTransactionTx records a coin.Transaction to the UnconfirmedTransactionPool if the txn is not
// already in the blockchain.
// The bool return value is whether or not the transaction was already in the pool.
// If the transaction violates hard or soft constraints, it is rejected, and error will not be nil.
// This method is only exported for use by the daemon gateway's InjectBroadcastTransaction method.
func (vs *Visor) InjectUserTransactionTx(tx *dbutil.Tx, txn coin.Transaction) (bool, *coin.SignedBlock, coin.UxArray, error) {
	if err := transaction.VerifySingleTxnUserConstraints(txn); err != nil {
		return false, nil, nil, err
	}

	head, inputs, err := vs.blockchain.VerifySingleTxnSoftHardConstraints(tx, txn, vs.Config.Distribution, params.UserVerifyTxn, transaction.TxnSigned)
	if err != nil {
		return false, nil, nil, err
	}

	known, softErr, err := vs.unconfirmed.InjectTransaction(tx, vs.blockchain, txn, vs.Config.Distribution, params.UserVerifyTxn)
	if softErr != nil {
		logger.WithError(softErr).Warning("InjectUserTransaction vs.unconfirmed.InjectTransaction returned a softErr unexpectedly")
	}

	return known, head, inputs, err
}

// GetTransaction returns a Transaction by hash.
func (vs *Visor) GetTransaction(txnHash cipher.SHA256) (*Transaction, error) {
	var txn *Transaction

	if err := vs.db.View("GetTransaction", func(tx *dbutil.Tx) error {
		var err error
		txn, err = vs.getTransaction(tx, txnHash)
		return err
	}); err != nil {
		return nil, err
	}

	return txn, nil
}

// GetTransactionWithInputs returns a Transaction by hash, along with the unspent outputs of its inputs
func (vs *Visor) GetTransactionWithInputs(txnHash cipher.SHA256) (*Transaction, []TransactionInput, error) {
	var txn *Transaction
	var inputs []TransactionInput

	if err := vs.db.View("GetTransactionWithInputs", func(tx *dbutil.Tx) error {
		var err error
		txn, err = vs.getTransaction(tx, txnHash)
		if err != nil {
			return err
		}

		if txn == nil {
			return nil
		}

		feeCalcTime, err := vs.getFeeCalcTimeForTransaction(tx, *txn)
		if err != nil {
			return err
		}
		if feeCalcTime == nil {
			return nil
		}

		inputs, err = vs.getTransactionInputs(tx, *feeCalcTime, txn.Transaction.In)
		return err
	}); err != nil {
		return nil, nil, err
	}

	return txn, inputs, nil
}

func (vs *Visor) getTransaction(tx *dbutil.Tx, txnHash cipher.SHA256) (*Transaction, error) {
	// Look in the unconfirmed pool
	utxn, err := vs.unconfirmed.Get(tx, txnHash)
	if err != nil {
		return nil, err
	}

	if utxn != nil {
		return &Transaction{
			Transaction: utxn.Transaction,
			Status:      NewUnconfirmedTransactionStatus(),
			Time:        uint64(timeutil.NanoToTime(utxn.Received).Unix()),
		}, nil
	}

	htxn, err := vs.history.GetTransaction(tx, txnHash)
	if err != nil {
		return nil, err
	}

	if htxn == nil {
		return nil, nil
	}

	headSeq, ok, err := vs.blockchain.HeadSeq(tx)
	if err != nil {
		return nil, err
	} else if !ok {
		return nil, errors.New("blockchain is empty but history has transactions")
	}

	b, err := vs.blockchain.GetSignedBlockBySeq(tx, htxn.BlockSeq)
	if err != nil {
		return nil, err
	}

	if b == nil {
		return nil, fmt.Errorf("found no block in seq %v", htxn.BlockSeq)
	}

	if headSeq < htxn.BlockSeq {
		return nil, fmt.Errorf("blockchain head seq %d is earlier than history txn seq %d", headSeq, htxn.BlockSeq)
	}

	confirms := headSeq - htxn.BlockSeq + 1
	return &Transaction{
		Transaction: htxn.Txn,
		Status:      NewConfirmedTransactionStatus(confirms, htxn.BlockSeq),
		Time:        b.Time(),
	}, nil
}

// TxFilter transaction filter type
type TxFilter interface {
	// Returns whether the transaction is matched
	Match(*Transaction) bool
}

// BaseFilter is a helper struct for generating TxFilter.
type BaseFilter struct {
	F func(tx *Transaction) bool
}

// Match matches the filter based upon F
func (f BaseFilter) Match(tx *Transaction) bool {
	return f.F(tx)
}

// NewAddrsFilter collects all addresses related transactions.
func NewAddrsFilter(addrs []cipher.Address) TxFilter {
	return AddrsFilter{Addrs: addrs}
}

// AddrsFilter filters by addresses
type AddrsFilter struct {
	Addrs []cipher.Address
}

// Match implements the TxFilter interface, this actually won't be used, only the 'Addrs' member is used.
func (af AddrsFilter) Match(tx *Transaction) bool { return true }

// ConfirmedTxFilter filters transactions base on whether they are confirmed.
type ConfirmedTxFilter struct {
	Confirmed bool
}

// Match implements the TxFilter interface, this actually won't be used, only the value of 'Confirmed' is used.
func (cf ConfirmedTxFilter) Match(tx *Transaction) bool {
	return tx.Status.Confirmed == cf.Confirmed
}

// NewConfirmedTxFilter collects the transaction whose 'Confirmed' status matchs the parameter passed in.
func NewConfirmedTxFilter(isConfirmed bool) TxFilter {
	return ConfirmedTxFilter{Confirmed: isConfirmed}
}

// GetTransactions returns transactions that can pass the filters with page.
// If no filters is provided, returns all transactions.
func (vs *Visor) GetTransactions(flts []TxFilter, order SortOrder, page *PageIndex) ([]Transaction, uint64, error) {
	var txns []Transaction
	var pages uint64
	if err := vs.db.View("GetTransactions", func(tx *dbutil.Tx) error {
		var err error
		txns, pages, err = vs.txns.GetTransactions(tx, flts, order, page)
		return err
	}); err != nil {
		return nil, 0, err
	}

	return txns, pages, nil
}

// GetTransactionsWithInputs is the same as GetTransactions but also returns verbose transaction input data
func (vs *Visor) GetTransactionsWithInputs(flts []TxFilter, order SortOrder, page *PageIndex) ([]Transaction, [][]TransactionInput, uint64, error) {
	var txns []Transaction
	var inputs [][]TransactionInput
	var pages uint64
	if err := vs.db.View("GetTransactionsWithInputs", func(tx *dbutil.Tx) error {
		var err error
		txns, pages, err = vs.txns.GetTransactions(tx, flts, order, page)
		if err != nil {
			return err
		}

		inputs = make([][]TransactionInput, len(txns))
		for i, txn := range txns {
			feeCalcTime, err := vs.getFeeCalcTimeForTransaction(tx, txn)
			if err != nil {
				return err
			}
			if feeCalcTime == nil {
				continue
			}

			txnInputs, err := vs.getTransactionInputs(tx, *feeCalcTime, txn.Transaction.In)
			if err != nil {
				return err
			}

			inputs[i] = txnInputs
		}

		return nil
	}); err != nil {
		return nil, nil, 0, err
	}

	return txns, inputs, pages, nil
}

// AddressBalances computes the total balance for cipher.Addresses and their coin.UxOuts
func (vs *Visor) AddressBalances(head *coin.SignedBlock, auxs coin.AddressUxOuts) (uint64, uint64, error) {
	prevTime := head.Time()
	var coins uint64
	var hours uint64
	for _, uxs := range auxs {
		for _, ux := range uxs {
			uxHours, err := ux.CoinHours(prevTime)
			if err != nil {
				return 0, 0, err
			}

			coins, err = mathutil.AddUint64(coins, ux.Body.Coins)
			if err != nil {
				return 0, 0, err
			}

			hours, err = mathutil.AddUint64(hours, uxHours)
			if err != nil {
				return 0, 0, err
			}
		}
	}
	return coins, hours, nil
}

// GetUnconfirmedTransactions gets all confirmed transactions of specific addresses
func (vs *Visor) GetUnconfirmedTransactions(filter func(UnconfirmedTransaction) bool) ([]UnconfirmedTransaction, error) {
	var txns []UnconfirmedTransaction

	if err := vs.db.View("GetUnconfirmedTransactions", func(tx *dbutil.Tx) error {
		var err error
		txns, err = vs.unconfirmed.GetFiltered(tx, filter)
		return err
	}); err != nil {
		return nil, err
	}

	return txns, nil
}

// GetUnconfirmedTransactionsVerbose gets all confirmed transactions of specific addresses
func (vs *Visor) GetUnconfirmedTransactionsVerbose(filter func(UnconfirmedTransaction) bool) ([]UnconfirmedTransaction, [][]TransactionInput, error) {
	var txns []UnconfirmedTransaction
	var inputs [][]TransactionInput

	if err := vs.db.View("GetUnconfirmedTransactionsVerbose", func(tx *dbutil.Tx) error {
		var err error
		txns, err = vs.unconfirmed.GetFiltered(tx, filter)
		if err != nil {
			return err
		}

		inputs, err = vs.getTransactionInputsForUnconfirmedTxns(tx, txns)

		return err
	}); err != nil {
		return nil, nil, err
	}

	if len(txns) == 0 {
		return nil, nil, nil
	}

	return txns, inputs, nil
}

// SendsToAddresses represents a filter that check if tx has output to the given addresses
func SendsToAddresses(addresses []cipher.Address) func(UnconfirmedTransaction) bool {
	return func(tx UnconfirmedTransaction) (isRelated bool) {
		for _, out := range tx.Transaction.Out {
			for _, address := range addresses {
				if out.Address == address {
					isRelated = true
					return
				}
			}
		}
		return
	}
}

// GetAllUnconfirmedTransactions returns all unconfirmed transactions
func (vs *Visor) GetAllUnconfirmedTransactions() ([]UnconfirmedTransaction, error) {
	var txns []UnconfirmedTransaction

	if err := vs.db.View("GetAllUnconfirmedTransactions", func(tx *dbutil.Tx) error {
		var err error
		txns, err = vs.unconfirmed.GetFiltered(tx, All)
		return err
	}); err != nil {
		return nil, err
	}

	return txns, nil
}

// GetAllUnconfirmedTransactionsVerbose returns all unconfirmed transactions with verbose transaction input data
func (vs *Visor) GetAllUnconfirmedTransactionsVerbose() ([]UnconfirmedTransaction, [][]TransactionInput, error) {
	var txns []UnconfirmedTransaction
	var inputs [][]TransactionInput

	if err := vs.db.View("GetAllUnconfirmedTransactionsVerbose", func(tx *dbutil.Tx) error {
		var err error
		txns, err = vs.unconfirmed.GetFiltered(tx, All)
		if err != nil {
			return err
		}

		inputs, err = vs.getTransactionInputsForUnconfirmedTxns(tx, txns)

		return err
	}); err != nil {
		return nil, nil, err
	}

	if len(txns) == 0 {
		return nil, nil, nil
	}

	return txns, inputs, nil
}

// getTransactionInputsForUnconfirmedTxns returns ReadableTransactionInputs for a set of UnconfirmedTransactions
func (vs *Visor) getTransactionInputsForUnconfirmedTxns(tx *dbutil.Tx, txns []UnconfirmedTransaction) ([][]TransactionInput, error) {
	if len(txns) == 0 {
		return nil, nil
	}

	// Use the current head time to calculate estimated coin hours of unconfirmed transactions
	headTime, err := vs.blockchain.Time(tx)
	if err != nil {
		return nil, err
	}

	inputs := make([][]TransactionInput, len(txns))
	for i, txn := range txns {
		if len(txn.Transaction.In) == 0 {
			logger.Critical().WithField("txid", txn.Transaction.Hash().Hex()).Warning("unconfirmed transaction has no inputs")
			continue
		}

		txnInputs, err := vs.getTransactionInputs(tx, headTime, txn.Transaction.In)
		if err != nil {
			return nil, err
		}

		inputs[i] = txnInputs
	}

	return inputs, nil
}

// getFeeCalcTimeForTransaction returns the time against which a transaction's fee should be calculated.
// The genesis block has no inputs and thus no fee to calculate, so it returns nil.
// A confirmed transaction's fee was calculated from the previous block's head time, when it was executed.
// An unconfirmed transaction's fee will be calculated from the current block head time, once executed.
func (vs *Visor) getFeeCalcTimeForTransaction(tx *dbutil.Tx, txn Transaction) (*uint64, error) {
	// The genesis block has no inputs to calculate, otherwise calculate the inputs
	if txn.Status.BlockSeq == 0 && txn.Status.Confirmed {
		return nil, nil
	}

	feeCalcTime := uint64(0)
	if txn.Status.Confirmed {
		// Use the previous block head to calculate the coin hours
		prevBlock, err := vs.blockchain.GetSignedBlockBySeq(tx, txn.Status.BlockSeq-1)
		if err != nil {
			return nil, err
		}

		if prevBlock == nil {
			err := fmt.Errorf("getFeeCalcTimeForTransaction: prevBlock seq=%d not found", txn.Status.BlockSeq-1)
			logger.Critical().WithError(err).Error("getFeeCalcTimeForTransaction")
			return nil, err
		}

		feeCalcTime = prevBlock.Block.Head.Time
	} else {
		// Use the current block head to calculate the coin hours
		var err error
		feeCalcTime, err = vs.blockchain.Time(tx)
		if err != nil {
			return nil, err
		}
	}

	return &feeCalcTime, nil
}

// GetAllValidUnconfirmedTxHashes returns all valid unconfirmed transaction hashes
func (vs *Visor) GetAllValidUnconfirmedTxHashes() ([]cipher.SHA256, error) {
	var hashes []cipher.SHA256

	if err := vs.db.View("GetAllValidUnconfirmedTxHashes", func(tx *dbutil.Tx) error {
		var err error
		hashes, err = vs.unconfirmed.GetHashes(tx, IsValid)
		return err
	}); err != nil {
		return nil, err
	}

	return hashes, nil
}

// GetConfirmedTransaction returns transaction, which has been already included in some block.
func (vs *Visor) GetConfirmedTransaction(txnHash cipher.SHA256) (*coin.Transaction, error) {
	var histTxn *historydb.Transaction

	if err := vs.db.View("GetConfirmedTransaction", func(tx *dbutil.Tx) error {
		var err error
		histTxn, err = vs.history.GetTransaction(tx, txnHash)
		return err
	}); err != nil {
		return nil, err
	}

	// Transaction not found.
	if histTxn == nil {
		return nil, nil
	}

	return &histTxn.Txn, nil
}

// GetTransactionsNum returns the total number of transactions that have been executed
func (vs *Visor) GetTransactionsNum() (uint64, error) {
	var (
		num uint64
		err error
	)

	if err := vs.db.View("GetTransactionsNum", func(tx *dbutil.Tx) error {
		num, err = vs.history.GetTransactionsNum(tx)
		return err
	}); err != nil {
		return 0, err
	}

	return num, nil
}

// GetSignedBlockByHash get block of specific hash header, return nil on not found.
func (vs *Visor) GetSignedBlockByHash(hash cipher.SHA256) (*coin.SignedBlock, error) {
	var sb *coin.SignedBlock

	if err := vs.db.View("GetSignedBlockByHash", func(tx *dbutil.Tx) error {
		var err error
		sb, err = vs.blockchain.GetSignedBlockByHash(tx, hash)
		return err
	}); err != nil {
		return nil, err
	}

	return sb, nil
}

// GetSignedBlockBySeq get block of specific seq, return nil on not found.
func (vs *Visor) GetSignedBlockBySeq(seq uint64) (*coin.SignedBlock, error) {
	var b *coin.SignedBlock

	if err := vs.db.View("GetSignedBlockBySeq", func(tx *dbutil.Tx) error {
		var err error
		b, err = vs.blockchain.GetSignedBlockBySeq(tx, seq)
		return err
	}); err != nil {
		return nil, err
	}

	return b, nil
}

// GetSignedBlockByHashVerbose returns a coin.SignedBlock and its transactions' input data for a given block hash
func (vs *Visor) GetSignedBlockByHashVerbose(hash cipher.SHA256) (*coin.SignedBlock, [][]TransactionInput, error) {
	var b *coin.SignedBlock
	var inputs [][]TransactionInput

	if err := vs.db.View("GetSignedBlockByHashVerbose", func(tx *dbutil.Tx) error {
		var err error
		b, inputs, err = vs.getBlockVerbose(tx, func(tx *dbutil.Tx) (*coin.SignedBlock, error) {
			return vs.blockchain.GetSignedBlockByHash(tx, hash)
		})
		return err
	}); err != nil {
		return nil, nil, err
	}

	return b, inputs, nil
}

// GetSignedBlockBySeqVerbose returns a coin.SignedBlock and its transactions' input data for a given block hash
func (vs *Visor) GetSignedBlockBySeqVerbose(seq uint64) (*coin.SignedBlock, [][]TransactionInput, error) {
	var b *coin.SignedBlock
	var inputs [][]TransactionInput

	if err := vs.db.View("GetSignedBlockBySeqVerbose", func(tx *dbutil.Tx) error {
		var err error
		b, inputs, err = vs.getBlockVerbose(tx, func(tx *dbutil.Tx) (*coin.SignedBlock, error) {
			return vs.blockchain.GetSignedBlockBySeq(tx, seq)
		})
		return err
	}); err != nil {
		return nil, nil, err
	}

	return b, inputs, nil
}

func (vs *Visor) getBlockVerbose(tx *dbutil.Tx, getBlock func(*dbutil.Tx) (*coin.SignedBlock, error)) (*coin.SignedBlock, [][]TransactionInput, error) {
	b, err := getBlock(tx)
	if err != nil {
		return nil, nil, err
	}

	if b == nil {
		return nil, nil, nil
	}

	inputs, err := vs.getBlockInputs(tx, b)
	if err != nil {
		return nil, nil, err
	}

	return b, inputs, nil
}

func (vs *Visor) getBlockInputs(tx *dbutil.Tx, b *coin.SignedBlock) ([][]TransactionInput, error) {
	if b == nil {
		return nil, nil
	}

	// The genesis block has no inputs to query or to calculate fees from
	if b.Block.Head.BkSeq == 0 {
		if len(b.Block.Body.Transactions) != 1 {
			logger.Panicf("Genesis block should have only 1 transaction (has %d)", len(b.Block.Body.Transactions))
		}

		if len(b.Block.Body.Transactions[0].In) != 0 {
			logger.Panic("Genesis block transaction should not have inputs")
		}

		inputs := make([][]TransactionInput, 1)

		return inputs, nil
	}

	// When a transaction was added to a block, its coinhour fee was
	// calculated based upon the time of the head block.
	// So we need to look at the previous block
	prevBlock, err := vs.blockchain.GetSignedBlockBySeq(tx, b.Head.BkSeq-1)
	if err != nil {
		return nil, err
	}

	if prevBlock == nil {
		err := fmt.Errorf("getBlockInputs: prevBlock seq %d not found", b.Head.BkSeq-1)
		logger.Critical().WithError(err).Error()
		return nil, err
	}

	var inputs [][]TransactionInput
	for _, txn := range b.Block.Body.Transactions {
		i, err := vs.getTransactionInputs(tx, prevBlock.Block.Head.Time, txn.In)
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, i)
	}

	return inputs, nil
}

// getTransactionInputs returns []TransactionInput for a given set of spent output hashes.
// feeCalcTime is the time against which to calculate the coinhours of the output
func (vs *Visor) getTransactionInputs(tx *dbutil.Tx, feeCalcTime uint64, inputs []cipher.SHA256) ([]TransactionInput, error) {
	if len(inputs) == 0 {
		err := errors.New("getTransactionInputs: inputs is empty only the genesis block transaction has no inputs, which shouldn't call this method")
		logger.WithError(err).Error()
		return nil, err
	}

	uxOuts, err := vs.history.GetUxOuts(tx, inputs)
	if err != nil {
		logger.WithError(err).Error("getTransactionInputs GetUxOuts failed")
		return nil, err
	}

	ret := make([]TransactionInput, len(inputs))
	for i, o := range uxOuts {
		r, err := NewTransactionInput(o.Out, feeCalcTime)
		if err != nil {
			logger.WithError(err).Error("getTransactionInputs NewTransactionInput failed")
			return nil, err
		}
		ret[i] = r
	}

	return ret, nil
}

// GetHeadBlock gets head block.
func (vs Visor) GetHeadBlock() (*coin.SignedBlock, error) {
	var b *coin.SignedBlock

	if err := vs.db.View("GetHeadBlock", func(tx *dbutil.Tx) error {
		var err error
		b, err = vs.blockchain.Head(tx)
		return err
	}); err != nil {
		return nil, err
	}

	return b, nil
}

// GetHeadBlockTime returns the time of the head block.
func (vs Visor) GetHeadBlockTime() (uint64, error) {
	var t uint64

	if err := vs.db.View("GetHeadBlockTime", func(tx *dbutil.Tx) error {
		var err error
		t, err = vs.blockchain.Time(tx)
		return err
	}); err != nil {
		return 0, err
	}

	return t, nil
}

// GetUxOutByID gets UxOut by hash id.
// return values:
//   first: uxout of the provided id, return nil if does not exist, no error would be returned.
//   second: current head block time
//   third: error
func (vs Visor) GetUxOutByID(id cipher.SHA256) (*historydb.UxOut, uint64, error) {
	var outs []historydb.UxOut
	var headTime uint64

	if err := vs.db.View("GetUxOutByID", func(tx *dbutil.Tx) error {
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		headTime = head.Time()

		outs, err = vs.history.GetUxOuts(tx, []cipher.SHA256{id})
		if err != nil {
			return err
		}

		return err
	}); err != nil {
		return nil, 0, err
	}

	if len(outs) == 0 {
		return nil, 0, nil
	}

	return &outs[0], headTime, nil
}

// GetSpentOutputsForAddresses gets all the spent outputs of a set of addresses
// return values:
//   first: addresses related uxouts
//   second: current head block time
//   third: error
func (vs Visor) GetSpentOutputsForAddresses(addresses []cipher.Address) ([][]historydb.UxOut, uint64, error) {
	out := make([][]historydb.UxOut, len(addresses))
	var headTime uint64

	if err := vs.db.View("GetSpentOutputsForAddresses", func(tx *dbutil.Tx) error {
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		headTime = head.Time()

		for i, addr := range addresses {
			addrUxOuts, err := vs.history.GetOutputsForAddress(tx, addr)
			if err != nil {
				return err
			}

			out[i] = addrUxOuts
		}

		return nil
	}); err != nil {
		return nil, 0, err
	}

	return out, headTime, nil
}

// RecvOfAddresses returns unconfirmed receiving uxouts of addresses
func (vs *Visor) RecvOfAddresses(addrs []cipher.Address) (coin.AddressUxOuts, error) {
	var uxouts coin.AddressUxOuts

	if err := vs.db.View("RecvOfAddresses", func(tx *dbutil.Tx) error {
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		uxouts, err = vs.unconfirmed.RecvOfAddresses(tx, head.Head, addrs)
		return err
	}); err != nil {
		return nil, err
	}

	return uxouts, nil
}

// GetIncomingOutputs returns all predicted outputs that are in pending tx pool
func (vs *Visor) GetIncomingOutputs() (coin.UxArray, error) {
	var uxa coin.UxArray

	if err := vs.db.View("GetIncomingOutputs", func(tx *dbutil.Tx) error {
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		uxa, err = vs.unconfirmed.GetIncomingOutputs(tx, head.Head)
		return err
	}); err != nil {
		return nil, err
	}

	return uxa, nil
}

// GetUnconfirmedTxn gets an unconfirmed transaction from the DB
func (vs *Visor) GetUnconfirmedTxn(hash cipher.SHA256) (*UnconfirmedTransaction, error) {
	var txn *UnconfirmedTransaction

	if err := vs.db.View("GetUnconfirmedTxn", func(tx *dbutil.Tx) error {
		var err error
		txn, err = vs.unconfirmed.Get(tx, hash)
		return err
	}); err != nil {
		return nil, err
	}

	return txn, nil
}

// FilterKnownUnconfirmed returns unconfirmed txn hashes with known ones removed
func (vs *Visor) FilterKnownUnconfirmed(txns []cipher.SHA256) ([]cipher.SHA256, error) {
	var hashes []cipher.SHA256

	if err := vs.db.View("FilterKnownUnconfirmed", func(tx *dbutil.Tx) error {
		var err error
		hashes, err = vs.unconfirmed.FilterKnown(tx, txns)
		return err
	}); err != nil {
		return nil, err
	}

	return hashes, nil
}

// GetKnownUnconfirmed returns unconfirmed txn hashes with known ones removed
func (vs *Visor) GetKnownUnconfirmed(txns []cipher.SHA256) (coin.Transactions, error) {
	var hashes coin.Transactions

	if err := vs.db.View("GetKnownUnconfirmed", func(tx *dbutil.Tx) error {
		var err error
		hashes, err = vs.unconfirmed.GetKnown(tx, txns)
		return err
	}); err != nil {
		return nil, err
	}

	return hashes, nil
}

// UnconfirmedSpendsOfAddresses returns all unconfirmed coin.UxOut spends of addresses
func (vs *Visor) UnconfirmedSpendsOfAddresses(addrs []cipher.Address) (coin.AddressUxOuts, error) {
	var outs coin.AddressUxOuts

	if err := vs.db.View("UnconfirmedSpendsOfAddresses", func(tx *dbutil.Tx) error {
		var err error
		outs, err = vs.unconfirmedSpendsOfAddresses(tx, addrs)
		return err
	}); err != nil {
		return nil, err
	}

	return outs, nil
}

// unconfirmedSpendsOfAddresses returns all unconfirmed coin.UxOut spends of addresses
func (vs *Visor) unconfirmedSpendsOfAddresses(tx *dbutil.Tx, addrs []cipher.Address) (coin.AddressUxOuts, error) {
	txns, err := vs.unconfirmed.AllRawTransactions(tx)
	if err != nil {
		return nil, err
	}

	var inputs []cipher.SHA256
	for _, txn := range txns {
		inputs = append(inputs, txn.In...)
	}

	uxa, err := vs.blockchain.Unspent().GetArray(tx, inputs)
	if err != nil {
		return nil, err
	}

	outs := make(coin.AddressUxOuts, len(addrs))

	addrm := make(map[cipher.Address]struct{}, len(addrs))
	for _, addr := range addrs {
		addrm[addr] = struct{}{}
	}

	for _, ux := range uxa {
		if _, ok := addrm[ux.Body.Address]; ok {
			outs[ux.Body.Address] = append(outs[ux.Body.Address], ux)
		}
	}

	return outs, nil
}

// SetTransactionsAnnounced updates announced time of specific tx
func (vs *Visor) SetTransactionsAnnounced(hashes map[cipher.SHA256]int64) error {
	if len(hashes) == 0 {
		return nil
	}

	return vs.db.Update("SetTransactionsAnnounced", func(tx *dbutil.Tx) error {
		return vs.unconfirmed.SetTransactionsAnnounced(tx, hashes)
	})
}

// GetBalanceOfAddresses returns balance pairs of given addreses
func (vs Visor) GetBalanceOfAddresses(addrs []cipher.Address) ([]wallet.BalancePair, error) {
	if len(addrs) == 0 {
		return nil, nil
	}

	auxs := make(coin.AddressUxOuts, len(addrs))
	recvUxs := make(coin.AddressUxOuts, len(addrs))
	var uxa coin.UxArray
	var head *coin.SignedBlock

	if err := vs.db.View("GetBalanceOfAddresses", func(tx *dbutil.Tx) error {
		var err error
		head, err = vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		// Get all transactions from the unconfirmed pool
		txns, err := vs.unconfirmed.AllRawTransactions(tx)
		if err != nil {
			return err
		}

		// Create predicted unspent outputs from the unconfirmed transactions
		recvUxs, err = txnOutputsForAddrs(head.Head, addrs, txns)
		if err != nil {
			return err
		}

		var inputs []cipher.SHA256
		for _, txn := range txns {
			inputs = append(inputs, txn.In...)
		}

		// Get unspents for the inputs being spent
		uxa, err = vs.blockchain.Unspent().GetArray(tx, inputs)
		if err != nil {
			return fmt.Errorf("GetArray failed when checking addresses balance: %v", err)
		}

		// Get unspents owned by the addresses
		auxs, err = vs.blockchain.Unspent().GetUnspentsOfAddrs(tx, addrs)
		if err != nil {
			return fmt.Errorf("GetUnspentsOfAddrs failed when checking addresses balance: %v", err)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	// Build all unconfirmed transaction inputs that are associated with the addresses
	spendUxs := make(coin.AddressUxOuts, len(addrs))

	addrm := make(map[cipher.Address]struct{}, len(addrs))
	for _, addr := range addrs {
		addrm[addr] = struct{}{}
	}

	for _, ux := range uxa {
		if _, ok := addrm[ux.Body.Address]; ok {
			spendUxs[ux.Body.Address] = append(spendUxs[ux.Body.Address], ux)
		}
	}

	var bps []wallet.BalancePair

	headTime := head.Time()
	for _, addr := range addrs {
		uxs, ok := auxs[addr]
		if !ok {
			bps = append(bps, wallet.BalancePair{})
			continue
		}

		outUxs := spendUxs[addr]
		inUxs := recvUxs[addr]
		predictedUxs := uxs.Sub(outUxs).Add(inUxs)

		coins, err := uxs.Coins()
		if err != nil {
			return nil, fmt.Errorf("uxs.Coins failed: %v", err)
		}

		coinHours, err := uxs.CoinHours(headTime)
		if err != nil {
			switch err {
			case coin.ErrAddEarnedCoinHoursAdditionOverflow:
				coinHours = 0
			default:
				return nil, fmt.Errorf("uxs.CoinHours failed: %v", err)
			}
		}

		pcoins, err := predictedUxs.Coins()
		if err != nil {
			return nil, fmt.Errorf("predictedUxs.Coins failed: %v", err)
		}

		pcoinHours, err := predictedUxs.CoinHours(headTime)
		if err != nil {
			switch err {
			case coin.ErrAddEarnedCoinHoursAdditionOverflow:
				coinHours = 0
			default:
				return nil, fmt.Errorf("predictedUxs.CoinHours failed: %v", err)
			}
		}

		bp := wallet.BalancePair{
			Confirmed: wallet.Balance{
				Coins: coins,
				Hours: coinHours,
			},
			Predicted: wallet.Balance{
				Coins: pcoins,
				Hours: pcoinHours,
			},
		}

		bps = append(bps, bp)
	}

	return bps, nil
}

// GetUnspentsOfAddrs returns unspent outputs of multiple addresses
func (vs *Visor) GetUnspentsOfAddrs(addrs []cipher.Address) (coin.AddressUxOuts, error) {
	var uxa coin.AddressUxOuts

	if err := vs.db.View("GetUnspentsOfAddrs", func(tx *dbutil.Tx) error {
		var err error
		uxa, err = vs.blockchain.Unspent().GetUnspentsOfAddrs(tx, addrs)
		return err
	}); err != nil {
		return nil, err
	}

	return uxa, nil
}

// VerifyTxnVerbose verifies a transaction, it returns transaction's input uxouts, whether the
// transaction is confirmed, and error if any
func (vs *Visor) VerifyTxnVerbose(txn *coin.Transaction, signed transaction.TxnSignedFlag) ([]TransactionInput, bool, error) {
	var uxa coin.UxArray
	var isTxnConfirmed bool
	var feeCalcTime uint64

	verifyErr := vs.db.View("VerifyTxnVerbose", func(tx *dbutil.Tx) error {
		head, err := vs.blockchain.Head(tx)
		if err != nil {
			return err
		}

		uxa, err = vs.blockchain.Unspent().GetArray(tx, txn.In)
		switch e := err.(type) {
		case nil:
			// For unconfirmed transactions, use the blockchain head time to calculate hours
			feeCalcTime = head.Time()

		case blockdb.ErrUnspentNotExist:
			// Gets uxouts of txn.In from historydb
			outs, err := vs.history.GetUxOuts(tx, txn.In)
			if err != nil {
				return err
			}

			if len(outs) == 0 {
				err = fmt.Errorf("transaction input of %s does not exist in either unspent pool or historydb", e.UxID)
				return transaction.NewErrTxnViolatesHardConstraint(err)
			}

			uxa = coin.UxArray{}
			for _, out := range outs {
				uxa = append(uxa, out.Out)
			}

			// Checks if the transaction is confirmed
			txnHash := txn.Hash()
			historyTxn, err := vs.history.GetTransaction(tx, txnHash)
			if err != nil {
				return fmt.Errorf("get transaction of %v from historydb failed: %v", txnHash, err)
			}

			if historyTxn != nil {
				// Transaction is confirmed
				isTxnConfirmed = true
			}

			// For confirmed transactions, use the previous block time to calculate hours and fees,
			// except for the genesis block which has no previous block and has no inputs nor fees.
			feeCalcTime = 0
			if historyTxn.BlockSeq > 0 {
				if isTxnConfirmed {
					prevBlock, err := vs.blockchain.GetSignedBlockBySeq(tx, historyTxn.BlockSeq-1)
					if err != nil {
						return err
					}
					if prevBlock == nil {
						return fmt.Errorf("VerifyTxnVerbose: previous block seq=%d not found", historyTxn.BlockSeq-1)
					}

					feeCalcTime = prevBlock.Block.Head.Time
				}
			}

			return nil
		default:
			return err
		}

		if err := transaction.VerifySingleTxnUserConstraints(*txn); err != nil {
			return err
		}

		if err := transaction.VerifySingleTxnSoftConstraints(*txn, feeCalcTime, uxa, vs.Config.Distribution, params.UserVerifyTxn); err != nil {
			return err
		}

		return transaction.VerifySingleTxnHardConstraints(*txn, head.Head, uxa, signed)
	})

	// If we were able to query the inputs, return the verbose inputs to the caller
	// even if the transaction failed validation
	var inputs []TransactionInput
	if len(uxa) != 0 && feeCalcTime != 0 {
		var err error
		inputs, err = NewTransactionInputs(uxa, feeCalcTime)
		if err != nil {
			return nil, isTxnConfirmed, err
		}
	}

	return inputs, isTxnConfirmed, verifyErr
}

// AddressCount returns the total number of addresses with unspents
func (vs *Visor) AddressCount() (uint64, error) {
	var count uint64
	if err := vs.db.View("AddressCount", func(tx *dbutil.Tx) error {
		var err error
		count, err = vs.blockchain.Unspent().AddressCount(tx)
		return err
	}); err != nil {
		return 0, err
	}

	return count, nil
}

// GetVerboseTransactionsForAddress returns verbose transaction data for a given address
// func (vs *Visor) GetVerboseTransactionsForAddress(a cipher.Address) ([]Transaction, [][]TransactionInput, error) {
// 	var txns []Transaction
// 	var inputs [][]TransactionInput

// 	if err := vs.db.View("GetVerboseTransactionsForAddress", func(tx *dbutil.Tx) error {
// 		var err error
// 		txns, _, err = vs.getTransactionsForAddresses(tx, []cipher.Address{a}, nil)
// 		if err != nil {
// 			logger.Errorf("GetVerboseTransactionsForAddress: vs.GetTransactionsForAddress failed: %v", err)
// 			return err
// 		}

// 		if len(txns) == 0 {
// 			return nil
// 		}

// 		head, err := vs.blockchain.Head(tx)
// 		if err != nil {
// 			logger.Errorf("GetVerboseTransactionsForAddress: vs.blockchain.Head failed: %v", err)
// 			return err
// 		}

// 		inputs = make([][]TransactionInput, len(txns))

// 		for i, txn := range txns {
// 			// If the txn is confirmed, use the time of the block previous
// 			// to the block in which the transaction was executed,
// 			// else use the head time for unconfirmed blocks.
// 			t := head.Time()
// 			if txn.Status.Confirmed && txn.Status.BlockSeq > 0 {
// 				prevBlock, err := vs.blockchain.GetSignedBlockBySeq(tx, txn.Status.BlockSeq-1)
// 				if err != nil {
// 					return err
// 				}

// 				if prevBlock == nil {
// 					return fmt.Errorf("GetVerboseTransactionsForAddress prevBlock seq=%d missing", txn.Status.BlockSeq-1)
// 				}

// 				t = prevBlock.Block.Head.Time
// 			}

// 			txnInputs := make([]TransactionInput, len(txn.Transaction.In))
// 			for j, inputID := range txn.Transaction.In {
// 				uxOuts, err := vs.history.GetUxOuts(tx, []cipher.SHA256{inputID})
// 				if err != nil {
// 					logger.Errorf("GetVerboseTransactionsForAddress: vs.history.GetUxOuts failed: %v", err)
// 					return err
// 				}
// 				if len(uxOuts) == 0 {
// 					err := fmt.Errorf("uxout of %v does not exist in history db", inputID.Hex())
// 					logger.Critical().Error(err)
// 					return err
// 				}

// 				input, err := NewTransactionInput(uxOuts[0].Out, t)
// 				if err != nil {
// 					logger.Errorf("GetVerboseTransactionsForAddress: NewTransactionInput failed: %v", err)
// 					return err
// 				}

// 				txnInputs[j] = input
// 			}

// 			inputs[i] = txnInputs
// 		}

// 		return nil
// 	}); err != nil {
// 		return nil, nil, err
// 	}

// 	return txns, inputs, nil
// }

// OutputsFilter used as optional arguments in GetUnspentOutputs method
type OutputsFilter func(outputs coin.UxArray) coin.UxArray

// FbyAddressesNotIncluded filters the unspent outputs that are not owned by the addresses
func FbyAddressesNotIncluded(addrs []cipher.Address) OutputsFilter {
	return func(outputs coin.UxArray) coin.UxArray {
		addrMatch := coin.UxArray{}
		addrMap := newAddrSet(addrs)

		for _, u := range outputs {
			if _, ok := addrMap[u.Body.Address]; !ok {
				addrMatch = append(addrMatch, u)
			}
		}
		return addrMatch
	}
}

// FbyAddresses filters the unspent outputs that owned by the addresses
func FbyAddresses(addrs []cipher.Address) OutputsFilter {
	return func(outputs coin.UxArray) coin.UxArray {
		addrMatch := coin.UxArray{}
		addrMap := newAddrSet(addrs)

		for _, u := range outputs {
			if _, ok := addrMap[u.Body.Address]; ok {
				addrMatch = append(addrMatch, u)
			}
		}
		return addrMatch
	}
}

// FbyHashes filters the unspent outputs that have hashes matched.
func FbyHashes(hashes []cipher.SHA256) OutputsFilter {
	return func(outputs coin.UxArray) coin.UxArray {
		hsMatch := coin.UxArray{}
		hsMap := newSHA256Set(hashes)

		for _, u := range outputs {
			if _, ok := hsMap[u.Hash()]; ok {
				hsMatch = append(hsMatch, u)
			}
		}
		return hsMatch
	}
}

func newAddrSet(keys []cipher.Address) map[cipher.Address]struct{} {
	s := make(map[cipher.Address]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

// newSHA256Set returns a map-based set for string lookup
func newSHA256Set(keys []cipher.SHA256) map[cipher.SHA256]struct{} {
	s := make(map[cipher.SHA256]struct{}, len(keys))
	for _, k := range keys {
		s[k] = struct{}{}
	}
	return s
}

// GetUnspentOutputsSummary gets unspent outputs and returns the filtered results,
// Note: all filters will be executed as the pending sequence in 'AND' mode.
func (vs *Visor) GetUnspentOutputsSummary(filters []OutputsFilter) (*UnspentOutputsSummary, error) {
	var confirmedOutputs []coin.UxOut
	var outgoingOutputs coin.UxArray
	var incomingOutputs coin.UxArray
	var head *coin.SignedBlock

	if err := vs.db.View("GetUnspentOutputsSummary", func(tx *dbutil.Tx) error {
		var err error
		head, err = vs.blockchain.Head(tx)
		if err != nil {
			return fmt.Errorf("vs.blockchain.Head failed: %v", err)
		}

		confirmedOutputs, err = vs.blockchain.Unspent().GetAll(tx)
		if err != nil {
			return fmt.Errorf("vs.blockchain.Unspent().GetAll failed: %v", err)
		}

		outgoingOutputs, err = vs.unconfirmedOutgoingOutputs(tx)
		if err != nil {
			return fmt.Errorf("vs.unconfirmedOutgoingOutputs failed: %v", err)
		}

		incomingOutputs, err = vs.unconfirmedIncomingOutputs(tx)
		if err != nil {
			return fmt.Errorf("vs.unconfirmedIncomingOutputs failed: %v", err)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	for _, flt := range filters {
		confirmedOutputs = flt(confirmedOutputs)
		outgoingOutputs = flt(outgoingOutputs)
		incomingOutputs = flt(incomingOutputs)
	}

	confirmed, err := NewUnspentOutputs(confirmedOutputs, head.Time())
	if err != nil {
		return nil, err
	}

	outgoing, err := NewUnspentOutputs(outgoingOutputs, head.Time())
	if err != nil {
		return nil, err
	}

	incoming, err := NewUnspentOutputs(incomingOutputs, head.Time())
	if err != nil {
		return nil, err
	}

	return &UnspentOutputsSummary{
		HeadBlock: head,
		Confirmed: confirmed,
		Outgoing:  outgoing,
		Incoming:  incoming,
	}, nil
}

// GetRichlist returns a Richlist
func (vs *Visor) GetRichlist(includeDistribution bool) (Richlist, error) {
	rbOuts, err := vs.GetUnspentOutputsSummary(nil)
	if err != nil {
		return nil, err
	}

	// Build a map from addresses to total coins held
	allAccounts := map[cipher.Address]uint64{}
	for _, out := range rbOuts.Confirmed {
		if _, ok := allAccounts[out.Body.Address]; ok {
			var err error
			allAccounts[out.Body.Address], err = mathutil.AddUint64(allAccounts[out.Body.Address], out.Body.Coins)
			if err != nil {
				return nil, err
			}
		} else {
			allAccounts[out.Body.Address] = out.Body.Coins
		}
	}

	lockedAddrs := vs.Config.Distribution.LockedAddressesDecoded()
	addrsMap := make(map[cipher.Address]struct{}, len(lockedAddrs))
	for _, a := range lockedAddrs {
		addrsMap[a] = struct{}{}
	}

	richlist, err := NewRichlist(allAccounts, addrsMap)
	if err != nil {
		return nil, err
	}

	if !includeDistribution {
		unlockedAddrs := vs.Config.Distribution.UnlockedAddressesDecoded()
		for _, a := range unlockedAddrs {
			addrsMap[a] = struct{}{}
		}
		richlist = richlist.FilterAddresses(addrsMap)
	}

	return richlist, nil
}

// WithUpdateTx executes a function inside of a db.Update transaction.
// This is exported for use by the daemon gateway's InjectBroadcastTransaction method.
// Do not use it for other purposes.
func (vs *Visor) WithUpdateTx(name string, f func(tx *dbutil.Tx) error) error {
	return vs.db.Update(name, func(tx *dbutil.Tx) error {
		return f(tx)
	})
}

// ScanWalletAddresses scan addresses ahead in a wallet to find addresses with transactions
func (vs *Visor) ScanWalletAddresses(wltID string, password []byte, num uint64) ([]cipher.Address, error) {
	return vs.wallets.ScanAddresses(wltID, password, num, vs.tf)
}

// TransactionsFinder returns a transactions finder
func (vs *Visor) TransactionsFinder() wallet.TransactionsFinder {
	return newTransactionsFinder(vs)
}

// TransactionsFinder implements the wallet.TransactionsFinder interface
type TransactionsFinder struct {
	db          *dbutil.DB
	history     Historyer
	unconfirmed UnconfirmedTransactionPooler
}

func newTransactionsFinder(v *Visor) *TransactionsFinder {
	return &TransactionsFinder{
		db:          v.db,
		history:     v.history,
		unconfirmed: v.unconfirmed,
	}
}

// AddressesActivity implements the methods of wallet.TransactionsFinder interface
func (tf *TransactionsFinder) AddressesActivity(addrs []cipher.Addresser) ([]bool, error) {
	if len(addrs) == 0 {
		return nil, nil
	}

	skyAddrs := make([]cipher.Address, len(addrs))
	// convert to skycoin addresses
	for i, a := range addrs {
		addr, ok := a.(cipher.Address)
		if !ok {
			return nil, errors.New("invalid skycoin address")
		}
		skyAddrs[i] = addr
	}

	active := make([]bool, len(addrs))
	addrsMap := make(map[cipher.Address]int, len(addrs))
	for i, a := range skyAddrs {
		addrsMap[a] = i
	}

	if len(addrsMap) != len(addrs) {
		return nil, errors.New("duplicates addresses not allowed")
	}

	if err := tf.db.View("AddressActivity", func(tx *dbutil.Tx) error {
		// Check if the addresses appear in the blockchain
		// scan from the last to first, break once find an address with transactions.
		for i := len(skyAddrs) - 1; i >= 0; i-- {
			ok, err := tf.history.AddressSeen(tx, skyAddrs[i])
			if err != nil {
				return err
			}

			if ok {
				active[i] = true
				break
			}
		}

		// Check if the addresses appears in the unconfirmed pool
		// NOTE: if this needs to be optimized, add an index to the unconfirmed pool
		return tf.unconfirmed.ForEach(tx, func(h cipher.SHA256, ut UnconfirmedTransaction) error {
			// Only transaction outputs need to be checked; if the address is associated
			// with an input, it must have appeared in a transaction in the blockchain history
			for _, o := range ut.Transaction.Out {
				if i, ok := addrsMap[o.Address]; ok {
					active[i] = true
				}
			}
			return nil
		})
	}); err != nil {
		return nil, err
	}

	return active, nil
}
