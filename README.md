# win_images_backup – Image backup CLI tool for Windows (Go)

A compact CLI tool that scans **image files** across local and connected drives, deduplicates via **SHA‑256**, and copies them to a **destination drive** (e.g., an external HDD) while **preserving the original path structure**. It includes progress output, optional logging, and a JSON index for resumable runs.

> **Executable name**: `win_images_backup.exe`

---

## Features
- Scans drives **A:–Z:** (the **destination drive is auto‑excluded**).
- Filters by image file extensions (customizable via `-ext`).
- Deduplication via **SHA‑256**; persistent index file `backup_index.json`.
- Preserves original path layout under the destination:  
  `<DEST>\<SOURCE-DRIVE>\<original\path\to\file>`
- Parallel processing (configurable number of workers).
- Windows long path / UNC support (`\\?\` and `\\?\UNC` when needed).
- Exclude whole drives and/or directories (names or absolute paths).
- Optional logfile for detailed diagnostics.

## Requirements
- Windows 10 or newer
- Go 1.20+ (recommended)

## Build
```powershell
# from the project directory
go build -o win_images_backup.exe
```

## Quick Start
```powershell
# Dry run (show planned copies only; does not write files)
.\win_images_backup.exe -dest E:\backup -dry-run -log dryrun.log

# Actual run with 4 workers and a logfile
.\win_images_backup.exe -dest E:\backup -workers 4 -log backup.log
```

## Usage
```
win_images_backup.exe [FLAGS]
```

### Flags

| Flag | Required | Default | Description |
|---|:--:|---|---|
| `-dest` | ✅ | – | Destination folder on the backup drive, e.g. `E:\backup`. Automatically excluded from scanning. |
| `-exclude` |  | – | Comma-separated **drive letters** to skip (e.g. `C,D`). |
| `-exclude-dirs` |  | – | Comma-separated **directory names** *or* **absolute paths** to skip. |
| `-no-default-excludes` |  | `false` | Disable default excludes: `Windows`, `Program Files`, `ProgramData`, `System Volume Information`, `AppData`, `Temp`, `tmp`, `$Recycle.Bin`. |
| `-ext` |  | predefined | Comma-separated extensions (with or without dot), e.g. `jpg,jpeg,png,heic,dng`. |
| `-workers` |  | `max(2, CPU/2)` | Number of parallel workers. Higher = faster, more IO. |
| `-index` |  | `backup_index.json` | File name / relative path of the hash index inside the destination folder. |
| `-dry-run` |  | `false` | Show what would be copied without copying. |
| `-log` |  | – | Write detailed logs to the given file, e.g. `backup.log`. |

### Default image extensions
`jpg, jpeg, png, gif, bmp, tiff, tif, webp, heic, heif, raw, cr2, nef, arw, rw2, orf, sr2, dng, psd, svg`

## How it works
- **Path structure:** Files are written under `-dest` with a **drive prefix**, e.g.:  
  `E:\backup\C\Users\Herwig\Pictures\Trip\img001.jpg`
- **Deduplication:** Before copying, the tool computes the file’s **SHA‑256**. If the hash exists in the index, the file is **skipped**.
- **Index:** The index is periodically updated and saved at the end as `-dest\backup_index.json`, enabling **resumable** runs.
- **Long paths / UNC:** Windows long paths (`\\?\`) and UNC paths are supported where needed.
- **Exclusions:** Besides `-exclude` (drives), use `-exclude-dirs` to omit directories by **name** or **absolute path**. The destination itself is always excluded.

## Examples
```powershell
# 1) Standard backup to E:, 4 workers, logfile
.\win_images_backup.exe -dest E:\backup -workers 4 -log backup.log

# 2) Dry run for validation
.\win_images_backup.exe -dest E:\backup -dry-run

# 3) Exclude specific drives (C:, D:)
.\win_images_backup.exe -dest E:\backup -exclude C,D

# 4) Exclude directories (names OR absolute paths)
.\win_images_backup.exe -dest E:\backup -exclude-dirs "Windows,Program Files,ProgramData,C:\Temp,\\server\share\Cache"

# 5) Limit to specific extensions
.\win_images_backup.exe -dest E:\backup -ext jpg,jpeg,png,heic,dng
```

## Logging & Progress
- Console shows: `Scanned | Queued | Copied | Skipped`
- With `-log`, detailed messages go to the specified file (warnings, errors, copy actions).

## Performance Tips
- This is **IO‑bound**. More workers increase throughput until the disks are saturated. Start with `CPU/2` and adjust.
- **Windows Defender / AV** may slow scanning. Consider excluding the destination folder or the EXE if appropriate.

## Troubleshooting
- **“Invalid name / Der angegebene Pfadname ist ungültig”**: Some special or installer-generated paths. Such files are skipped/logged.
- **Access denied**: System folders or reparse points may block reads. Run as Administrator or exclude with `-exclude-dirs`.
- **USB sleep/timeouts**: On external drives, consider disabling power-saving options.

## Destination Layout
```
E:\backup\
  ├─ backup_index.json
  ├─ C\Users\...\img001.jpg
  └─ D\Fotos\...\bild.png
```

## Development
- Written in Go. No external runtime dependencies.
- Linting / formatting: `go fmt ./...`

## Contributing
Issues and PRs are welcome. When filing an issue, include a log excerpt, Windows version, and the exact CLI command you ran.

## License
Choose a license (e.g., MIT) and add a `LICENSE` file.
