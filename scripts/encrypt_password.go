package main

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
    "flag"
    "fmt"
    "io"
    "os"
)

func Encrypt(key, text string) (string, error) {
    if len(key) != 32 {
        return "", fmt.Errorf("key must be 32 bytes long for AES-256, got %d", len(key))
    }

    block, err := aes.NewCipher([]byte(key))
    if err != nil {
        return "", fmt.Errorf("creating cipher: %v", err)
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", fmt.Errorf("creating GCM: %v", err)
    }

    nonce := make([]byte, gcm.NonceSize())
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return "", fmt.Errorf("generating nonce: %v", err)
    }

    ciphertext := gcm.Seal(nil, nonce, []byte(text), nil)
    return base64.StdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func main() {
    key := flag.String("key", "", "32-byte encryption key (required)")
    text := flag.String("text", "", "Text to encrypt (required)")
    flag.Parse()

    if *key == "" || *text == "" {
        fmt.Println("Usage: go run encrypt_password.go -key <32-byte-key> -text <plaintext>")
        fmt.Println("Example: go run encrypt_password.go -key \"32-byte-long-secret-key-here!!\" -text \"mypassword\"")
        os.Exit(1)
    }

    encrypted, err := Encrypt(*key, *text)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Encryption failed: %v\n", err)
        os.Exit(1)
    }

    if _, err := fmt.Printf("Encrypted value: %s\n", encrypted); err != nil {
        fmt.Fprintf(os.Stderr, "Failed to print output: %v\n", err)
        os.Exit(1)
    }
    if _, err := fmt.Println("Copy this value into your config.yml for fields like db_passwd or basic_auth.password."); err != nil {
        fmt.Fprintf(os.Stderr, "Failed to print output: %v\n", err)
        os.Exit(1)
    }
}