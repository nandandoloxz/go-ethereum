#!/bin/bash
set -e
echo $POSTGRES_USER
echo $POSTGRES_DB
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE DATABASE shyftdb;
    \connect shyftdb;
    CREATE TABLE IF NOT EXISTS blocks (
        hash text primary key,
        coinbase text,
        gasUsed numeric,
        gasLimit numeric,
        txCount numeric,
        uncleCount numeric,
        age timestamp,
        parentHash text,
        uncleHash text,
        difficulty bigint,
        size text,
        nonce numeric,
        rewards numeric,
        number bigint
        );
    CREATE TABLE IF NOT EXISTS txs (
        txHash text primary key unique,
        to_addr text,
        from_addr text,
        blockhash text references blocks(hash),
        blocknumber text,
        amount numeric,
        gasprice numeric,
        gas numeric,
        gasLimit numeric,
        txFee numeric,
        nonce numeric,
        txStatus text,
        isContract bool,
        age timestamp,
        data bytea
    );

    CREATE TABLE IF NOT EXISTS accounts (
        addr text primary key unique,
        balance numeric,
        txCountAccount numeric
    );

    CREATE TABLE IF NOT EXISTS contracts (
        txHash text
    );

    CREATE TABLE IF NOT EXISTS internalTxs (
        id SERIAL PRIMARY KEY,
        txHash text references txs(txHash),
        type text,
        to_addr text,
        from_addr text,
        amount text,
        gas numeric,
        gasUsed numeric,
        time text,
        input text,
        output text
    );
EOSQL
