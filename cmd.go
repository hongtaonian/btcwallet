/*
 * Copyright (c) 2013 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"errors"
	"fmt"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/tx"
	"github.com/conformal/btcwallet/wallet"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcws"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	// ErrNoWallet describes an error where a wallet does not exist and
	// must be created first.
	ErrNoWallet = errors.New("wallet file does not exist")

	// ErrNoUtxos describes an error where the wallet file was successfully
	// read, but the UTXO file was not.  To properly handle this error,
	// a rescan should be done since the wallet creation block.
	ErrNoUtxos = errors.New("utxo file cannot be read")

	// ErrNoTxs describes an error where the wallet and UTXO files were
	// successfully read, but the TX history file was not.  It is up to
	// the caller whether this necessitates a rescan or not.
	ErrNoTxs = errors.New("tx file cannot be read")

	cfg *config

	curBlock = struct {
		sync.RWMutex
		wallet.BlockStamp
	}{
		BlockStamp: wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		},
	}
)

// accountdir returns the directory path which holds an account's wallet, utxo,
// and tx files.
func accountdir(cfg *config, account string) string {
	var wname string
	if account == "" {
		wname = "btcwallet"
	} else {
		wname = fmt.Sprintf("btcwallet-%s", account)
	}

	return filepath.Join(cfg.DataDir, wname)
}

// checkCreateAccountDir checks that path exists and is a directory.
// If path does not exist, it is created.
func checkCreateAccountDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Attempt data directory creation
			if err = os.MkdirAll(path, 0700); err != nil {
				return fmt.Errorf("cannot create account directory: %s", err)
			}
		} else {
			return fmt.Errorf("error checking account directory: %s", err)
		}
	} else {
		if !fi.IsDir() {
			return fmt.Errorf("path '%s' is not a directory", cfg.DataDir)
		}
	}
	return nil
}

// OpenAccount opens an account described by account in the data
// directory specified by cfg.  If the wallet does not exist, ErrNoWallet
// is returned as an error.
//
// Wallets opened from this function are not set to track against a
// btcd connection.
func OpenAccount(cfg *config, account string) (*Account, error) {
	adir := accountdir(cfg, account)
	if err := checkCreateAccountDir(adir); err != nil {
		return nil, err
	}

	wfilepath := filepath.Join(adir, "wallet.bin")
	utxofilepath := filepath.Join(adir, "utxo.bin")
	txfilepath := filepath.Join(adir, "tx.bin")
	var wfile, utxofile, txfile *os.File

	// Read wallet file.
	wfile, err := os.Open(wfilepath)
	if err != nil {
		if os.IsNotExist(err) {
			// Must create and save wallet first.
			return nil, ErrNoWallet
		}
		return nil, fmt.Errorf("cannot open wallet file: %s", err)
	}
	defer wfile.Close()

	wlt := new(wallet.Wallet)
	if _, err = wlt.ReadFrom(wfile); err != nil {
		return nil, fmt.Errorf("cannot read wallet: %s", err)
	}

	a := &Account{
		Wallet: wlt,
		name:   account,
	}

	// Read utxo file.  If this fails, return a ErrNoUtxos error so a
	// rescan can be done since the wallet creation block.
	var utxos tx.UtxoStore
	if utxofile, err = os.Open(utxofilepath); err != nil {
		log.Errorf("cannot open utxo file: %s", err)
		return a, ErrNoUtxos
	}
	defer utxofile.Close()
	if _, err = utxos.ReadFrom(utxofile); err != nil {
		log.Errorf("cannot read utxo file: %s", err)
		return a, ErrNoUtxos
	}
	a.UtxoStore.s = utxos

	// Read tx file.  If this fails, return a ErrNoTxs error and let
	// the caller decide if a rescan is necessary.
	if txfile, err = os.Open(txfilepath); err != nil {
		log.Errorf("cannot open tx file: %s", err)
		return a, ErrNoTxs
	}
	defer txfile.Close()
	var txs tx.TxStore
	if _, err = txs.ReadFrom(txfile); err != nil {
		log.Errorf("cannot read tx file: %s", err)
		return a, ErrNoTxs
	}
	a.TxStore.s = txs

	return a, nil
}

// GetCurBlock returns the blockchain height and SHA hash of the most
// recently seen block.  If no blocks have been seen since btcd has
// connected, btcd is queried for the current block height and hash.
func GetCurBlock() (bs wallet.BlockStamp, err error) {
	curBlock.RLock()
	bs = curBlock.BlockStamp
	curBlock.RUnlock()
	if bs.Height != int32(btcutil.BlockHeightUnknown) {
		return bs, nil
	}

	// This is a hack and may result in races, but we need to make
	// sure that btcd is connected and sending a message will succeed,
	// or this will block forever. A better solution is to return an
	// error to the reply handler immediately if btcd is disconnected.
	if !btcdConnected.b {
		return wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		}, errors.New("current block unavailable")
	}

	n := <-NewJSONID
	cmd := btcws.NewGetBestBlockCmd(fmt.Sprintf("btcwallet(%v)", n))
	mcmd, err := cmd.MarshalJSON()
	if err != nil {
		return wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		}, errors.New("cannot ask for best block")
	}

	c := make(chan *struct {
		hash   *btcwire.ShaHash
		height int32
	})

	replyHandlers.Lock()
	replyHandlers.m[n] = func(result interface{}, e *btcjson.Error) bool {
		if e != nil {
			c <- nil
			return true
		}
		m, ok := result.(map[string]interface{})
		if !ok {
			c <- nil
			return true
		}
		hashBE, ok := m["hash"].(string)
		if !ok {
			c <- nil
			return true
		}
		hash, err := btcwire.NewShaHashFromStr(hashBE)
		if err != nil {
			c <- nil
			return true
		}
		fheight, ok := m["height"].(float64)
		if !ok {
			c <- nil
			return true
		}
		c <- &struct {
			hash   *btcwire.ShaHash
			height int32
		}{
			hash:   hash,
			height: int32(fheight),
		}
		return true
	}
	replyHandlers.Unlock()

	// send message
	btcdMsgs <- mcmd

	// Block until reply is ready.
	reply, ok := <-c
	if !ok || reply == nil {
		return wallet.BlockStamp{
			Height: int32(btcutil.BlockHeightUnknown),
		}, errors.New("current block unavailable")
	}

	curBlock.Lock()
	if reply.height > curBlock.BlockStamp.Height {
		bs = wallet.BlockStamp{
			Height: reply.height,
			Hash:   *reply.hash,
		}
		curBlock.BlockStamp = bs
	}
	curBlock.Unlock()
	return bs, nil
}

// NewJSONID is used to receive the next unique JSON ID for btcd
// requests, starting from zero and incrementing by one after each
// read.
var NewJSONID = make(chan uint64)

// JSONIDGenerator sends incremental integers across a channel.  This
// is meant to provide a unique value for the JSON ID field for btcd
// messages.
func JSONIDGenerator(c chan uint64) {
	var n uint64
	for {
		c <- n
		n++
	}
}

func main() {
	// Initialize logging and setup deferred flushing to ensure all
	// outstanding messages are written on shutdown
	loggers := setLogLevel(defaultLogLevel)
	defer func() {
		for _, logger := range loggers {
			logger.Flush()
		}
	}()

	tcfg, _, err := loadConfig()
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
	cfg = tcfg

	// Change the logging level if needed.
	if cfg.DebugLevel != defaultLogLevel {
		loggers = setLogLevel(cfg.DebugLevel)
	}

	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			log.Infof("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			log.Errorf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Open default account
	a, err := OpenAccount(cfg, "")
	switch err {
	case ErrNoTxs:
		// Do nothing special for now.  This will be implemented when
		// the tx history file is properly written.
		accounts.Lock()
		accounts.m[""] = a
		accounts.Unlock()

	case ErrNoUtxos:
		// Add wallet, but mark wallet as needing a full rescan since
		// the wallet creation block.  This will take place when btcd
		// connects.
		accounts.Lock()
		accounts.m[""] = a
		accounts.Unlock()
		a.fullRescan = true

	case nil:
		accounts.Lock()
		accounts.m[""] = a
		accounts.Unlock()

	default:
		log.Warnf("cannot open wallet: %v", err)
	}

	// Read CA file to verify a btcd TLS connection.
	cafile, err := ioutil.ReadFile(cfg.CAFile)
	if err != nil {
		log.Errorf("cannot open CA file: %v", err)
		os.Exit(1)
	}

	// Start account disk syncer goroutine.
	go DirtyAccountSyncer()

	go func() {
		s, err := newServer()
		if err != nil {
			log.Errorf("Unable to create HTTP server: %v", err)
			os.Exit(1)
		}

		// Start HTTP server to listen and send messages to frontend and btcd
		// backend.  Try reconnection if connection failed.
		s.Start()
	}()

	// Begin generating new IDs for JSON calls.
	go JSONIDGenerator(NewJSONID)

	for {
		replies := make(chan error)
		done := make(chan int)
		go func() {
			BtcdConnect(cafile, replies)
			close(done)
		}()
	selectLoop:
		for {
			select {
			case <-done:
				break selectLoop
			case err := <-replies:
				switch err {
				case ErrConnRefused:
					btcdConnected.c <- false
					log.Info("btcd connection refused, retying in 5 seconds")
					time.Sleep(5 * time.Second)
				case ErrConnLost:
					btcdConnected.c <- false
					log.Info("btcd connection lost, retrying in 5 seconds")
					time.Sleep(5 * time.Second)
				case nil:
					btcdConnected.c <- true
					log.Info("Established connection to btcd.")
				default:
					log.Infof("Unhandled error: %v", err)
				}
			}
		}
	}
}
