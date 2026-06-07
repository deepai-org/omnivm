package main

import (
    "fmt"
    "net/http"
)

func statusLabel(code int) string {
    if code >= http.StatusBadRequest {
        return fmt.Sprintf("error:%d", code)
    }
    return fmt.Sprintf("ok:%d", code)
}

var compatibilityStatus = statusLabel(http.StatusAccepted)

func main() {
    fmt.Println(statusLabel(http.StatusOK))
}
