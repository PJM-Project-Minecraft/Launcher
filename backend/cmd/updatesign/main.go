// Command updatesign — оффлайн-подпись бинарников автообновления лаунчера (Ed25519).
//
// Приватный ключ живёт ТОЛЬКО на релиз-боксе (в git и на сервере его нет), публичный
// вшивается в лаунчер при сборке (LAUNCHER_UPDATE_PUBKEY). Так скомпрометированный
// backend или зеркало не подсунут игрокам чужой бинарник: SHA-256 приходит тем же
// каналом, что и файл, и сам по себе подлинность не доказывает — подпись доказывает.
//
//	updatesign keygen [keyfile]          — сгенерировать ключ; seed → keyfile (0600),
//	                                        публичный ключ (hex) → stdout.
//	updatesign pubkey -key keyfile       — напечатать публичный ключ (hex) из приватного.
//	updatesign sign   -key keyfile <bin> — напечатать hex-подпись бинарника (128 hex).
//
// Подпись совместима с ed25519-dalek (verify_strict) в лаунчере — обычный Ed25519 (RFC 8032).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2:])
	case "pubkey":
		pubkey(os.Args[2:])
	case "sign":
		sign(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: updatesign keygen [keyfile] | pubkey -key keyfile | sign -key keyfile <binary>")
	os.Exit(2)
}

func keygen(args []string) {
	keyfile := "update-signing.key"
	if len(args) > 0 {
		keyfile = args[0]
	}
	if _, err := os.Stat(keyfile); err == nil {
		fatalf("отказ: %s уже существует (не перезаписываю приватный ключ)", keyfile)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatalf("генерация ключа: %v", err)
	}
	if err := os.WriteFile(keyfile, []byte(hex.EncodeToString(priv.Seed())), 0o600); err != nil {
		fatalf("запись ключа: %v", err)
	}
	fmt.Fprintf(os.Stderr, "приватный ключ сохранён: %s (храните ТОЛЬКО на релиз-боксе, в git НЕ коммитить)\n", keyfile)
	fmt.Fprintln(os.Stderr, "публичный ключ (LAUNCHER_UPDATE_PUBKEY при сборке лаунчера):")
	fmt.Println(hex.EncodeToString(pub))
}

func pubkey(args []string) {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	keyfile := fs.String("key", "", "путь к приватному ключу")
	_ = fs.Parse(args)
	if *keyfile == "" {
		usage()
	}
	priv := loadKey(*keyfile)
	fmt.Println(hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
}

func sign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyfile := fs.String("key", "", "путь к приватному ключу")
	_ = fs.Parse(args)
	if *keyfile == "" || fs.NArg() != 1 {
		usage()
	}
	priv := loadKey(*keyfile)
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fatalf("чтение бинарника: %v", err)
	}
	fmt.Println(hex.EncodeToString(ed25519.Sign(priv, data)))
}

func loadKey(keyfile string) ed25519.PrivateKey {
	raw, err := os.ReadFile(keyfile)
	if err != nil {
		fatalf("чтение ключа: %v", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		fatalf("некорректный приватный ключ (ожидается %d hex-байт seed)", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
