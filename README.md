# CookieScope — Defensive Cookie Security Lab

CookieScope is an educational security-testing tool written in **Go** that helps developers, students, and security teams understand browser-cookie formats, archive processing, and session-security risks in controlled environments.

The project is designed for **authorized defensive testing**, using synthetic cookie fixtures, locally generated test archives, and explicitly authorized lab datasets.

## Features

| Area              | Details                                                                                           |
| ----------------- | ------------------------------------------------------------------------------------------------- |
| Archive testing   | Process ZIP, RAR, and 7z test archives, including nested and multi-part fixtures                  |
| Cookie formats    | Learn to parse Netscape-format text, JSON examples, and synthetic browser-cookie database records |
| Test datasets     | Generate and analyze fake cookies that cannot authenticate to real accounts                       |
| Domain analysis   | Validate domain matching, subdomain handling, and cookie scope                                    |
| Cookie quality    | Identify expired, empty, malformed, or insecure test-cookie records                               |
| Security checks   | Inspect Secure, HttpOnly, and SameSite attributes where available                                 |
| Reporting         | Produce sanitized JSON and text summaries for security reviews                                    |
| Offline CLI       | Run reproducible tests locally without Telegram credentials                                       |
| Automated testing | Include unit tests, race detection, static analysis, and build verification                       |

## Educational Objectives

* Understand how browsers represent cookies and session identifiers.
* Learn why stolen session cookies can create account-takeover risks.
* Test archive parsers against malformed, encrypted, nested, and incomplete test fixtures.
* Identify insecure cookie attributes and configuration mistakes.
* Practice data minimization, redaction, and secure temporary-file handling.
* Learn how to protect authentication sessions and respond to suspected cookie theft.

## Safe Testing Workflow

1. Generate a local dataset containing synthetic cookie values.
2. Package the fixtures into ZIP, RAR, or 7z archives.
3. Run the offline parser against the test data.
4. Validate domain matching, expiry handling, and cookie-attribute checks.
5. Generate a sanitized report containing findings and test results.
6. Run automated tests to verify that malformed input and archive-processing errors are handled safely.

## Security Boundaries

CookieScope is intended for systems and datasets you own or are explicitly authorized to test.

* Use synthetic session identifiers rather than real authentication tokens.
* Never attempt to log in using extracted session cookies.
* Do not collect, redistribute, or expose real users' authentication credentials.
* Redact sensitive values from logs, reports, and test output.
* Restrict archive sizes, nesting depth, processing time, and filesystem access.
* Remove temporary files securely and avoid retaining unnecessary sensitive data.

## Testing

```bash
go test -count=1 ./...
go test -race ./...
go vet ./...
gofmt -l .
go build ./...
```

## Intended Use

This project supports cybersecurity education, secure software development, defensive cookie analysis, and authorized laboratory testing. It is not intended to recover or reuse real users' authentication sessions.
