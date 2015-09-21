// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/ethereum/ethash"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/pow"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/hashicorp/golang-lru"
)

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func thePow() pow.PoW {
	pow, _ := ethash.NewForTesting()
	return pow
}

func theChainManager(db ethdb.Database, t *testing.T) *ChainManager {
	var eventMux event.TypeMux
	WriteTestNetGenesisBlock(db, 0)
	chainMan, err := NewChainManager(db, thePow(), &eventMux)
	if err != nil {
		t.Error("failed creating chainmanager:", err)
		t.FailNow()
		return nil
	}
	blockMan := NewBlockProcessor(db, nil, chainMan, &eventMux)
	chainMan.SetProcessor(blockMan)

	return chainMan
}

// Test fork of length N starting from block i
func testFork(t *testing.T, processor *BlockProcessor, i, n int, full bool, comparator func(td1, td2 *big.Int)) {
	// Copy old chain up to #i into a new db
	db, processor2, err := newCanonical(i, full)
	if err != nil {
		t.Fatal("could not make new canonical in testFork", err)
	}
	// Assert the chains have the same header/block at #i
	var hash1, hash2 common.Hash
	if full {
		hash1 = processor.bc.GetBlockByNumber(uint64(i)).Hash()
		hash2 = processor2.bc.GetBlockByNumber(uint64(i)).Hash()
	} else {
		hash1 = processor.bc.GetHeaderByNumber(uint64(i)).Hash()
		hash2 = processor2.bc.GetHeaderByNumber(uint64(i)).Hash()
	}
	if hash1 != hash2 {
		t.Errorf("chain content mismatch at %d: have hash %v, want hash %v", i, hash2, hash1)
	}
	// Extend the newly created chain
	var (
		blockChainB  []*types.Block
		headerChainB []*types.Header
	)
	if full {
		blockChainB = makeBlockChain(processor2.bc.CurrentBlock(), n, db, forkSeed)
		if _, err := processor2.bc.InsertChain(blockChainB); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}
	} else {
		headerChainB = makeHeaderChain(processor2.bc.CurrentHeader(), n, db, forkSeed)
		if _, err := processor2.bc.InsertHeaderChain(headerChainB, true); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}
	}
	// Sanity check that the forked chain can be imported into the original
	var tdPre, tdPost *big.Int

	if full {
		tdPre = processor.bc.GetTd(processor.bc.CurrentBlock().Hash())
		if err := testBlockChainImport(blockChainB, processor); err != nil {
			t.Fatalf("failed to import forked block chain: %v", err)
		}
		tdPost = processor.bc.GetTd(blockChainB[len(blockChainB)-1].Hash())
	} else {
		tdPre = processor.bc.GetTd(processor.bc.CurrentHeader().Hash())
		if err := testHeaderChainImport(headerChainB, processor); err != nil {
			t.Fatalf("failed to import forked header chain: %v", err)
		}
		tdPost = processor.bc.GetTd(headerChainB[len(headerChainB)-1].Hash())
	}
	// Compare the total difficulties of the chains
	comparator(tdPre, tdPost)
}

func printChain(bc *ChainManager) {
	for i := bc.CurrentBlock().Number().Uint64(); i > 0; i-- {
		b := bc.GetBlockByNumber(uint64(i))
		fmt.Printf("\t%x %v\n", b.Hash(), b.Difficulty())
	}
}

// testBlockChainImport tries to process a chain of blocks, writing them into
// the database if successful.
func testBlockChainImport(chain []*types.Block, processor *BlockProcessor) error {
	for _, block := range chain {
		// Try and process the block
		if _, _, err := processor.Process(block); err != nil {
			if IsKnownBlockErr(err) {
				continue
			}
			return err
		}
		// Manually insert the block into the database, but don't reorganize (allows subsequent testing)
		processor.bc.mu.Lock()
		WriteTd(processor.chainDb, block.Hash(), new(big.Int).Add(block.Difficulty(), processor.bc.GetTd(block.ParentHash())))
		WriteBlock(processor.chainDb, block)
		processor.bc.mu.Unlock()
	}
	return nil
}

// testHeaderChainImport tries to process a chain of header, writing them into
// the database if successful.
func testHeaderChainImport(chain []*types.Header, processor *BlockProcessor) error {
	for _, header := range chain {
		// Try and validate the header
		if err := processor.ValidateHeader(header, false, false); err != nil {
			return err
		}
		// Manually insert the header into the database, but don't reorganize (allows subsequent testing)
		processor.bc.mu.Lock()
		WriteTd(processor.chainDb, header.Hash(), new(big.Int).Add(header.Difficulty, processor.bc.GetTd(header.ParentHash)))
		WriteHeader(processor.chainDb, header)
		processor.bc.mu.Unlock()
	}
	return nil
}

func loadChain(fn string, t *testing.T) (types.Blocks, error) {
	fh, err := os.OpenFile(filepath.Join("..", "_data", fn), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	var chain types.Blocks
	if err := rlp.Decode(fh, &chain); err != nil {
		return nil, err
	}

	return chain, nil
}

func insertChain(done chan bool, chainMan *ChainManager, chain types.Blocks, t *testing.T) {
	_, err := chainMan.InsertChain(chain)
	if err != nil {
		fmt.Println(err)
		t.FailNow()
	}
	done <- true
}

// Tests that given a starting canonical chain of a given size, it can be extended
// with various length chains.
func TestExtendCanonicalHeaders(t *testing.T) { testExtendCanonical(t, false) }
func TestExtendCanonicalBlocks(t *testing.T)  { testExtendCanonical(t, true) }

func testExtendCanonical(t *testing.T, full bool) {
	length := 5

	// Make first chain starting from genesis
	_, processor, err := newCanonical(length, full)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	// Define the difficulty comparator
	better := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) <= 0 {
			t.Errorf("total difficulty mismatch: have %v, expected more than %v", td2, td1)
		}
	}
	// Start fork from current height
	testFork(t, processor, length, 1, full, better)
	testFork(t, processor, length, 2, full, better)
	testFork(t, processor, length, 5, full, better)
	testFork(t, processor, length, 10, full, better)
}

// Tests that given a starting canonical chain of a given size, creating shorter
// forks do not take canonical ownership.
func TestShorterForkHeaders(t *testing.T) { testShorterFork(t, false) }
func TestShorterForkBlocks(t *testing.T)  { testShorterFork(t, true) }

func testShorterFork(t *testing.T, full bool) {
	length := 10

	// Make first chain starting from genesis
	_, processor, err := newCanonical(length, full)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	// Define the difficulty comparator
	worse := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) >= 0 {
			t.Errorf("total difficulty mismatch: have %v, expected less than %v", td2, td1)
		}
	}
	// Sum of numbers must be less than `length` for this to be a shorter fork
	testFork(t, processor, 0, 3, full, worse)
	testFork(t, processor, 0, 7, full, worse)
	testFork(t, processor, 1, 1, full, worse)
	testFork(t, processor, 1, 7, full, worse)
	testFork(t, processor, 5, 3, full, worse)
	testFork(t, processor, 5, 4, full, worse)
}

// Tests that given a starting canonical chain of a given size, creating longer
// forks do take canonical ownership.
func TestLongerForkHeaders(t *testing.T) { testLongerFork(t, false) }
func TestLongerForkBlocks(t *testing.T)  { testLongerFork(t, true) }

func testLongerFork(t *testing.T, full bool) {
	length := 10

	// Make first chain starting from genesis
	_, processor, err := newCanonical(length, full)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	// Define the difficulty comparator
	better := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) <= 0 {
			t.Errorf("total difficulty mismatch: have %v, expected more than %v", td2, td1)
		}
	}
	// Sum of numbers must be greater than `length` for this to be a longer fork
	testFork(t, processor, 0, 11, full, better)
	testFork(t, processor, 0, 15, full, better)
	testFork(t, processor, 1, 10, full, better)
	testFork(t, processor, 1, 12, full, better)
	testFork(t, processor, 5, 6, full, better)
	testFork(t, processor, 5, 8, full, better)
}

// Tests that given a starting canonical chain of a given size, creating equal
// forks do take canonical ownership.
func TestEqualForkHeaders(t *testing.T) { testEqualFork(t, false) }
func TestEqualForkBlocks(t *testing.T)  { testEqualFork(t, true) }

func testEqualFork(t *testing.T, full bool) {
	length := 10

	// Make first chain starting from genesis
	_, processor, err := newCanonical(length, full)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	// Define the difficulty comparator
	equal := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", td2, td1)
		}
	}
	// Sum of numbers must be equal to `length` for this to be an equal fork
	testFork(t, processor, 0, 10, full, equal)
	testFork(t, processor, 1, 9, full, equal)
	testFork(t, processor, 2, 8, full, equal)
	testFork(t, processor, 5, 5, full, equal)
	testFork(t, processor, 6, 4, full, equal)
	testFork(t, processor, 9, 1, full, equal)
}

// Tests that chains missing links do not get accepted by the processor.
func TestBrokenHeaderChain(t *testing.T) { testBrokenChain(t, false) }
func TestBrokenBlockChain(t *testing.T)  { testBrokenChain(t, true) }

func testBrokenChain(t *testing.T, full bool) {
	// Make chain starting from genesis
	db, processor, err := newCanonical(10, full)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	// Create a forked chain, and try to insert with a missing link
	if full {
		chain := makeBlockChain(processor.bc.CurrentBlock(), 5, db, forkSeed)[1:]
		if err := testBlockChainImport(chain, processor); err == nil {
			t.Errorf("broken block chain not reported")
		}
	} else {
		chain := makeHeaderChain(processor.bc.CurrentHeader(), 5, db, forkSeed)[1:]
		if err := testHeaderChainImport(chain, processor); err == nil {
			t.Errorf("broken header chain not reported")
		}
	}
}

func TestChainInsertions(t *testing.T) {
	t.Skip("Skipped: outdated test files")

	db, _ := ethdb.NewMemDatabase()

	chain1, err := loadChain("valid1", t)
	if err != nil {
		fmt.Println(err)
		t.FailNow()
	}

	chain2, err := loadChain("valid2", t)
	if err != nil {
		fmt.Println(err)
		t.FailNow()
	}

	chainMan := theChainManager(db, t)

	const max = 2
	done := make(chan bool, max)

	go insertChain(done, chainMan, chain1, t)
	go insertChain(done, chainMan, chain2, t)

	for i := 0; i < max; i++ {
		<-done
	}

	if chain2[len(chain2)-1].Hash() != chainMan.CurrentBlock().Hash() {
		t.Error("chain2 is canonical and shouldn't be")
	}

	if chain1[len(chain1)-1].Hash() != chainMan.CurrentBlock().Hash() {
		t.Error("chain1 isn't canonical and should be")
	}
}

func TestChainMultipleInsertions(t *testing.T) {
	t.Skip("Skipped: outdated test files")

	db, _ := ethdb.NewMemDatabase()

	const max = 4
	chains := make([]types.Blocks, max)
	var longest int
	for i := 0; i < max; i++ {
		var err error
		name := "valid" + strconv.Itoa(i+1)
		chains[i], err = loadChain(name, t)
		if len(chains[i]) >= len(chains[longest]) {
			longest = i
		}
		fmt.Println("loaded", name, "with a length of", len(chains[i]))
		if err != nil {
			fmt.Println(err)
			t.FailNow()
		}
	}

	chainMan := theChainManager(db, t)

	done := make(chan bool, max)
	for i, chain := range chains {
		// XXX the go routine would otherwise reference the same (chain[3]) variable and fail
		i := i
		chain := chain
		go func() {
			insertChain(done, chainMan, chain, t)
			fmt.Println(i, "done")
		}()
	}

	for i := 0; i < max; i++ {
		<-done
	}

	if chains[longest][len(chains[longest])-1].Hash() != chainMan.CurrentBlock().Hash() {
		t.Error("Invalid canonical chain")
	}
}

type bproc struct{}

func (bproc) Process(*types.Block) (state.Logs, types.Receipts, error) { return nil, nil, nil }

func makeHeaderChainWithDiff(genesis *types.Block, d []int, seed byte) []*types.Header {
	blocks := makeBlockChainWithDiff(genesis, d, seed)
	headers := make([]*types.Header, len(blocks))
	for i, block := range blocks {
		headers[i] = block.Header()
	}
	return headers
}

func makeBlockChainWithDiff(genesis *types.Block, d []int, seed byte) []*types.Block {
	var chain []*types.Block
	for i, difficulty := range d {
		header := &types.Header{
			Coinbase:   common.Address{seed},
			Number:     big.NewInt(int64(i + 1)),
			Difficulty: big.NewInt(int64(difficulty)),
		}
		if i == 0 {
			header.ParentHash = genesis.Hash()
		} else {
			header.ParentHash = chain[i-1].Hash()
		}
		block := types.NewBlockWithHeader(header)
		chain = append(chain, block)
	}
	return chain
}

func chm(genesis *types.Block, db ethdb.Database) *ChainManager {
	var eventMux event.TypeMux
	bc := &ChainManager{chainDb: db, genesisBlock: genesis, eventMux: &eventMux, pow: FakePow{}}
	bc.headerCache, _ = lru.New(100)
	bc.bodyCache, _ = lru.New(100)
	bc.bodyRLPCache, _ = lru.New(100)
	bc.tdCache, _ = lru.New(100)
	bc.blockCache, _ = lru.New(100)
	bc.futureBlocks, _ = lru.New(100)
	bc.processor = bproc{}
	bc.ResetWithGenesisBlock(genesis)

	return bc
}

// Tests that reorganizing a long difficult chain after a short easy one
// overwrites the canonical numbers and links in the database.
func TestReorgLongHeaders(t *testing.T) { testReorgLong(t, false) }
func TestReorgLongBlocks(t *testing.T)  { testReorgLong(t, true) }

func testReorgLong(t *testing.T, full bool) {
	testReorg(t, []int{1, 2, 4}, []int{1, 2, 3, 4}, 10, full)
}

// Tests that reorganizing a short difficult chain after a long easy one
// overwrites the canonical numbers and links in the database.
func TestReorgShortHeaders(t *testing.T) { testReorgShort(t, false) }
func TestReorgShortBlocks(t *testing.T)  { testReorgShort(t, true) }

func testReorgShort(t *testing.T, full bool) {
	testReorg(t, []int{1, 2, 3, 4}, []int{1, 10}, 11, full)
}

func testReorg(t *testing.T, first, second []int, td int64, full bool) {
	// Create a pristine block chain
	db, _ := ethdb.NewMemDatabase()
	genesis, _ := WriteTestNetGenesisBlock(db, 0)
	bc := chm(genesis, db)

	// Insert an easy and a difficult chain afterwards
	if full {
		bc.InsertChain(makeBlockChainWithDiff(genesis, first, 11))
		bc.InsertChain(makeBlockChainWithDiff(genesis, second, 22))
	} else {
		bc.InsertHeaderChain(makeHeaderChainWithDiff(genesis, first, 11), false)
		bc.InsertHeaderChain(makeHeaderChainWithDiff(genesis, second, 22), false)
	}
	// Check that the chain is valid number and link wise
	if full {
		prev := bc.CurrentBlock()
		for block := bc.GetBlockByNumber(bc.CurrentBlock().NumberU64() - 1); block.NumberU64() != 0; prev, block = block, bc.GetBlockByNumber(block.NumberU64()-1) {
			if prev.ParentHash() != block.Hash() {
				t.Errorf("parent block hash mismatch: have %x, want %x", prev.ParentHash(), block.Hash())
			}
		}
	} else {
		prev := bc.CurrentHeader()
		for header := bc.GetHeaderByNumber(bc.CurrentHeader().Number.Uint64() - 1); header.Number.Uint64() != 0; prev, header = header, bc.GetHeaderByNumber(header.Number.Uint64()-1) {
			if prev.ParentHash != header.Hash() {
				t.Errorf("parent header hash mismatch: have %x, want %x", prev.ParentHash, header.Hash())
			}
		}
	}
	// Make sure the chain total difficulty is the correct one
	want := new(big.Int).Add(genesis.Difficulty(), big.NewInt(td))
	if full {
		if have := bc.GetTd(bc.CurrentBlock().Hash()); have.Cmp(want) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", have, want)
		}
	} else {
		if have := bc.GetTd(bc.CurrentHeader().Hash()); have.Cmp(want) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", have, want)
		}
	}
}

// Tests chain insertions in the face of one entity containing an invalid nonce.
func TestHeadersInsertNonceError(t *testing.T) { testInsertNonceError(t, false) }
func TestBlocksInsertNonceError(t *testing.T)  { testInsertNonceError(t, true) }

func testInsertNonceError(t *testing.T, full bool) {
	for i := 1; i < 25 && !t.Failed(); i++ {
		// Create a pristine chain and database
		db, processor, err := newCanonical(0, full)
		if err != nil {
			t.Fatalf("failed to create pristine chain: %v", err)
		}
		bc := processor.bc

		// Create and insert a chain with a failing nonce
		var (
			failAt   int
			failRes  int
			failNum  uint64
			failHash common.Hash
		)
		if full {
			blocks := makeBlockChain(processor.bc.CurrentBlock(), i, db, 0)

			failAt = rand.Int() % len(blocks)
			failNum = blocks[failAt].NumberU64()
			failHash = blocks[failAt].Hash()

			processor.bc.pow = failPow{failNum}
			failRes, err = processor.bc.InsertChain(blocks)
		} else {
			headers := makeHeaderChain(processor.bc.CurrentHeader(), i, db, 0)

			failAt = rand.Int() % len(headers)
			failNum = headers[failAt].Number.Uint64()
			failHash = headers[failAt].Hash()

			processor.bc.pow = failPow{failNum}
			failRes, err = processor.bc.InsertHeaderChain(headers, true)
		}
		// Check that the returned error indicates the nonce failure.
		if failRes != failAt {
			t.Errorf("test %d: failure index mismatch: have %d, want %d", i, failRes, failAt)
		}
		if !IsBlockNonceErr(err) {
			t.Fatalf("test %d: error mismatch: have %v, want nonce error", i, err)
		}
		nerr := err.(*BlockNonceErr)
		if nerr.Number.Uint64() != failNum {
			t.Errorf("test %d: number mismatch: have %v, want %v", i, nerr.Number, failNum)
		}
		if nerr.Hash != failHash {
			t.Errorf("test %d: hash mismatch: have %x, want %x", i, nerr.Hash[:4], failHash[:4])
		}
		// Check that all no blocks after the failing block have been inserted.
		for j := 0; j < i-failAt; j++ {
			if full {
				if block := bc.GetBlockByNumber(failNum + uint64(j)); block != nil {
					t.Errorf("test %d: invalid block in chain: %v", i, block)
				}
			} else {
				if header := bc.GetHeaderByNumber(failNum + uint64(j)); header != nil {
					t.Errorf("test %d: invalid header in chain: %v", i, header)
				}
			}
		}
	}
}

// Tests that chain reorganizations handle transaction removals and reinsertions.
func TestChainTxReorgs(t *testing.T) {
	params.MinGasLimit = big.NewInt(125000)      // Minimum the gas limit may ever be.
	params.GenesisGasLimit = big.NewInt(3141592) // Gas limit of the Genesis block.

	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		addr3   = crypto.PubkeyToAddress(key3.PublicKey)
		db, _   = ethdb.NewMemDatabase()
	)
	genesis := WriteGenesisBlockForTesting(db,
		GenesisAccount{addr1, big.NewInt(1000000)},
		GenesisAccount{addr2, big.NewInt(1000000)},
		GenesisAccount{addr3, big.NewInt(1000000)},
	)
	// Create two transactions shared between the chains:
	//  - postponed: transaction included at a later block in the forked chain
	//  - swapped: transaction included at the same block number in the forked chain
	postponed, _ := types.NewTransaction(0, addr1, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key1)
	swapped, _ := types.NewTransaction(1, addr1, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key1)

	// Create two transactions that will be dropped by the forked chain:
	//  - pastDrop: transaction dropped retroactively from a past block
	//  - freshDrop: transaction dropped exactly at the block where the reorg is detected
	var pastDrop, freshDrop *types.Transaction

	// Create three transactions that will be added in the forked chain:
	//  - pastAdd:   transaction added before the reorganiztion is detected
	//  - freshAdd:  transaction added at the exact block the reorg is detected
	//  - futureAdd: transaction added after the reorg has already finished
	var pastAdd, freshAdd, futureAdd *types.Transaction

	chain := GenerateChain(genesis, db, 3, func(i int, gen *BlockGen) {
		switch i {
		case 0:
			pastDrop, _ = types.NewTransaction(gen.TxNonce(addr2), addr2, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key2)

			gen.AddTx(pastDrop)  // This transaction will be dropped in the fork from below the split point
			gen.AddTx(postponed) // This transaction will be postponed till block #3 in the fork

		case 2:
			freshDrop, _ = types.NewTransaction(gen.TxNonce(addr2), addr2, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key2)

			gen.AddTx(freshDrop) // This transaction will be dropped in the fork from exactly at the split point
			gen.AddTx(swapped)   // This transaction will be swapped out at the exact height

			gen.OffsetTime(9) // Lower the block difficulty to simulate a weaker chain
		}
	})
	// Import the chain. This runs all block validation rules.
	evmux := &event.TypeMux{}
	chainman, _ := NewChainManager(db, FakePow{}, evmux)
	chainman.SetProcessor(NewBlockProcessor(db, FakePow{}, chainman, evmux))
	if i, err := chainman.InsertChain(chain); err != nil {
		t.Fatalf("failed to insert original chain[%d]: %v", i, err)
	}

	// overwrite the old chain
	chain = GenerateChain(genesis, db, 5, func(i int, gen *BlockGen) {
		switch i {
		case 0:
			pastAdd, _ = types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key3)
			gen.AddTx(pastAdd) // This transaction needs to be injected during reorg

		case 2:
			gen.AddTx(postponed) // This transaction was postponed from block #1 in the original chain
			gen.AddTx(swapped)   // This transaction was swapped from the exact current spot in the original chain

			freshAdd, _ = types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key3)
			gen.AddTx(freshAdd) // This transaction will be added exactly at reorg time

		case 3:
			futureAdd, _ = types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, nil, nil).SignECDSA(key3)
			gen.AddTx(futureAdd) // This transaction will be added after a full reorg
		}
	})
	if _, err := chainman.InsertChain(chain); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	// removed tx
	for i, tx := range (types.Transactions{pastDrop, freshDrop}) {
		if GetTransaction(db, tx.Hash()) != nil {
			t.Errorf("drop %d: tx found while shouldn't have been", i)
		}
		if GetReceipt(db, tx.Hash()) != nil {
			t.Errorf("drop %d: receipt found while shouldn't have been", i)
		}
	}
	// added tx
	for i, tx := range (types.Transactions{pastAdd, freshAdd, futureAdd}) {
		if GetTransaction(db, tx.Hash()) == nil {
			t.Errorf("add %d: expected tx to be found", i)
		}
		if GetReceipt(db, tx.Hash()) == nil {
			t.Errorf("add %d: expected receipt to be found", i)
		}
	}
	// shared tx
	for i, tx := range (types.Transactions{postponed, swapped}) {
		if GetTransaction(db, tx.Hash()) == nil {
			t.Errorf("share %d: expected tx to be found", i)
		}
		if GetReceipt(db, tx.Hash()) == nil {
			t.Errorf("share %d: expected receipt to be found", i)
		}
	}
}
