/*
 * sshbox is a utility to encrypt a file using SSH keys.
 */
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"flag"
	"fmt"
	"github.com/gokyle/cryptobox/secretbox"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"regexp"
)

type boxPackage struct {
	LockedKey []byte
	Box       []byte
}

type sshPublicKey struct {
	Algorithm []byte
	Modulus   []byte
	Exponent  []byte
}

var pubkeyRegexp = regexp.MustCompile("^ssh-rsa (\\S+).*$")
var remoteCheck = regexp.MustCompile("^https?://")

func main() {
	flArmour := flag.Bool("a", false, "ASCII armour the box")
	flDecrypt := flag.Bool("d", false, "decrypt file")
	flEncrypt := flag.Bool("e", false, "encrypt file")
	flKeyFile := flag.String("k", "", "SSH key file")
	flag.Parse()

	if *flDecrypt && *flEncrypt {
		fmt.Println("[!] only one of -d or -e can be specified!")
		os.Exit(1)
	}

	if flag.NArg() != 2 {
		fmt.Println("[!] source and target must both be specified.")
		fmt.Printf("\t%s [options] source target\n", os.Args[0])
		os.Exit(1)
	}
	source := flag.Args()[0]
	target := flag.Args()[1]

	if *flKeyFile == "" {
		fmt.Println("[!] no key was specified!\n")
		os.Exit(1)
	}

	remote := remoteCheck.MatchString(*flKeyFile)
	if remote {
		if *flDecrypt {
			fmt.Println("[+] remotely fetching private keys is not allowed.")
			os.Exit(1)
		}
		fmt.Println("[+] will fetch key")
	}

	if *flEncrypt {
		err := encrypt(source, target, *flKeyFile, !remote, *flArmour)
		if err != nil {
			fmt.Println("[!] failed.")
			os.Exit(1)
		}
		fmt.Println("[+] success")
		os.Exit(0)
	} else {
		err := decrypt(source, target, *flKeyFile, *flArmour)
		if err != nil {
			fmt.Println("[!] failed.")
			os.Exit(1)
		}
		fmt.Println("[+] success")
		os.Exit(0)
	}
}

// fetchKey retrieves the raw data for a key, either via file or an HTTP get.
func fetchKey(name string, local bool) (kb []byte, err error) {
	if local {
		kb, err = ioutil.ReadFile(name)
		if err != nil {
			fmt.Println("[!]", err.Error())
		}
	} else {
		var resp *http.Response
		resp, err = http.Get(name)
		if err != nil {
			fmt.Println("[!] failed to fetch key:", err.Error())
			return
		}
		defer resp.Body.Close()

		kb, err = ioutil.ReadAll(resp.Body)
	}
	return
}

// Decode a byte slice into an RSA public key.
func loadPublicKey(name string, local bool) (key *rsa.PublicKey, err error) {
	kb64, err := fetchKey(name, local)

	kb64 = pubkeyRegexp.ReplaceAll(kb64, []byte("$1"))
	kb := make([]byte, base64.StdEncoding.DecodedLen(len(kb64)))
	_, err = base64.StdEncoding.Decode(kb, kb64)
	if err != nil {
		fmt.Println("[!] couldn't decode public key:", err.Error())
		return
	}
	buf := bytes.NewBuffer(kb)
	var pubKey sshPublicKey
	var length int32

	err = binary.Read(buf, binary.BigEndian, &length)
	if err != nil {
		fmt.Println("[!] failed to read public key:", err.Error())
		return
	}

	pubKey.Algorithm = make([]byte, length)
	_, err = io.ReadFull(buf, pubKey.Algorithm)
	if err != nil {
		fmt.Println("[!] failed to decode public key:", err.Error())
		return
	}
	if string(pubKey.Algorithm) != "ssh-rsa" {
		fmt.Println("[!] invalid public key.")
		err = fmt.Errorf("invalid public key")
		return
	}

	err = binary.Read(buf, binary.BigEndian, &length)
	if err != nil {
		fmt.Println("[!] failed to read public key:", err.Error())
		return
	}
	pubKey.Exponent = make([]byte, length)
	_, err = io.ReadFull(buf, pubKey.Exponent)
	if err != nil {
		fmt.Println("[!] failed to decode public key:", err.Error())
		return
	}

	err = binary.Read(buf, binary.BigEndian, &length)
	if err != nil {
		fmt.Println("[!] failed to read public key:", err.Error())
		return
	}
	pubKey.Modulus = make([]byte, length)
	_, err = io.ReadFull(buf, pubKey.Modulus)
	if err != nil {
		fmt.Println("[!] failed to decode public key:", err.Error())
		return
	}

	key = new(rsa.PublicKey)
	key.N = new(big.Int).SetBytes(pubKey.Modulus)
	key.E = int(new(big.Int).SetBytes(pubKey.Exponent).Int64())
	return
}

// Decode a byte slice into an RSA private key. Note that OpenSSH
// private keys are in PEM format.
func loadPrivateKey(name string) (key *rsa.PrivateKey, err error) {
	kb, err := fetchKey(name, true)
	block, _ := pem.Decode(kb)
	if block == nil {
		fmt.Println("[!] couldn't decode key file.")
		os.Exit(1)
	} else if block.Type != "RSA PRIVATE KEY" {
		fmt.Println("[!] key is not a private key.")
		os.Exit(1)
	}

	key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	return

}

// Generate a random box key, encrypt the key to the RSA public key,
// package the box appropriately, and write it out to a file.
func encrypt(in, out, keyfile string, local, armour bool) (err error) {
	pub, err := loadPublicKey(keyfile, local)
	if err != nil {
		return
	}
	boxKey, err := secretbox.GenerateKey()
	if err != nil {
		fmt.Println("[!] failed to generate the box key.")
		return
	}

	hash := sha256.New()
	lockedKey, err := rsa.EncryptOAEP(hash, rand.Reader, pub, boxKey, nil)
	if err != nil {
		fmt.Println("[!] RSA encryption failed:", err.Error())
		return
	}

	message, err := ioutil.ReadFile(in)
	if err != nil {
		fmt.Println("[!]", err.Error())
		return
	}

	box, ok := secretbox.Seal(message, boxKey)
	if !ok {
		fmt.Println("[!] failed to seal the message.")
		err = fmt.Errorf("sealing failure")
		return
	}
	pkg, err := packageBox(lockedKey, box, armour)
	if err != nil {
		return
	}

	err = ioutil.WriteFile(out, pkg, 0644)
	if err != nil {
		fmt.Println("[!]", err.Error())
	}
	return
}

// packageBox actually handles boxing. It can output either PEM-encoded or
// DER-encoded boxes.
func packageBox(lockedKey, box []byte, armour bool) (pkg []byte, err error) {
	var pkgBox = boxPackage{lockedKey, box}

	pkg, err = asn1.Marshal(pkgBox)
	if err != nil {
		fmt.Println("[!] couldn't package the box")
		return
	}

	if armour {
		var block pem.Block
		block.Type = "SSHBOX ENCRYPTED FILE"
		block.Bytes = pkg
		pkg = pem.EncodeToMemory(&block)
	}
	return
}

// Decrypt loads the box, recovers the key using the RSA private key, open
// the box, and write the message to a file.
func decrypt(in, out, keyfile string, armour bool) (err error) {
	key, err := loadPrivateKey(keyfile)
	if err != nil {
		return
	}

	pkg, err := ioutil.ReadFile(in)
	if err != nil {
		fmt.Println("[!]", err.Error())
		return
	}

	lockedKey, box, err := unpackageBox(pkg)
	if err != nil {
		return
	}

	hash := sha256.New()
	boxKey, err := rsa.DecryptOAEP(hash, rand.Reader, key, lockedKey, nil)
	if err != nil {
		fmt.Println("[!] RSA decryption failed:", err.Error())
		return
	}

	message, ok := secretbox.Open(box, boxKey)
	if !ok {
		fmt.Println("[!] failed to open box.")
		err = fmt.Errorf("opening box failed")
		return
	}
	err = ioutil.WriteFile(out, message, 0644)
	return
}

// unpackageBox handles the loading of a box; it first attempts to decode the
// box as a DER-encoded box. If this fails, it attempts to decode the box as
// a PEM-encoded box.
func unpackageBox(pkg []byte) (lockedKey, box []byte, err error) {
	var pkgStruct boxPackage

	_, err = asn1.Unmarshal(pkg, &pkgStruct)
	if err == nil {
		return pkgStruct.LockedKey, pkgStruct.Box, nil
	}

	block, _ := pem.Decode(pkg)
	if block == nil || block.Type != "SSHBOX ENCRYPTED FILE" {
		fmt.Println("[!] invalid box.")
		err = fmt.Errorf("invalid box")
		return
	}
	_, err = asn1.Unmarshal(block.Bytes, &pkgStruct)
	return pkgStruct.LockedKey, pkgStruct.Box, err
}