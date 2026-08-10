package control

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

type packageVersionMetadata struct {
	StoragePath string
	TargetPath  string
	SHA256      string
	Size        int64
	CreatedAt   time.Time
}

const maxScannedPackageBytes = 512 << 20

func (s *Store) ListPackageVersions(ctx context.Context, instanceID, packageID string) ([]domain.AddonPackageVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,package_id,instance_id,version,sha256,size,target_path,storage_path,created_at
		FROM addon_package_versions WHERE instance_id=?1 AND package_id=?2 ORDER BY created_at DESC,id DESC`, instanceID, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AddonPackageVersion, 0)
	for rows.Next() {
		var item domain.AddonPackageVersion
		var size int64
		if err := rows.Scan(&item.ID, &item.PackageID, &item.InstanceID, &item.Version, &item.SHA256, &size,
			&item.TargetPath, &item.StoragePath, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Size = uint64(size)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) PackageVersionForRollback(ctx context.Context, instanceID, packageID, versionID string) (domain.AddonPackage, domain.AddonPackageVersion, error) {
	var item domain.AddonPackage
	var lastJobID string
	err := s.db.QueryRowContext(ctx, `SELECT id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,
		COALESCE(installed_by,''),installed_at,COALESCE(last_job_id,'')
		FROM addon_packages WHERE id=?1 AND instance_id=?2`, packageID, instanceID).Scan(
		&item.ID, &item.InstanceID, &item.Name, &item.Kind, &item.Version, &item.Source, &item.SourceID, &item.SHA256,
		&item.Path, &item.Enabled, &item.Risk, &item.InstalledBy, &item.InstalledAt, &lastJobID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AddonPackage{}, domain.AddonPackageVersion{}, ErrNotFound
	}
	if err != nil {
		return domain.AddonPackage{}, domain.AddonPackageVersion{}, err
	}
	var version domain.AddonPackageVersion
	var size int64
	err = s.db.QueryRowContext(ctx, `SELECT id,package_id,instance_id,version,sha256,size,target_path,storage_path,created_at
		FROM addon_package_versions WHERE id=?1 AND package_id=?2 AND instance_id=?3`, versionID, packageID, instanceID).Scan(
		&version.ID, &version.PackageID, &version.InstanceID, &version.Version, &version.SHA256, &size,
		&version.TargetPath, &version.StoragePath, &version.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AddonPackage{}, domain.AddonPackageVersion{}, ErrNotFound
	}
	version.Size = uint64(size)
	return item, version, err
}

func (s *Store) PackageForDelete(ctx context.Context, instanceID, packageID string) (domain.AddonPackage, string, []string, error) {
	pkg, path, err := s.PackageForToggle(ctx, instanceID, packageID)
	if err != nil {
		return domain.AddonPackage{}, "", nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT storage_path FROM addon_package_versions
		WHERE instance_id=?1 AND package_id=?2 ORDER BY created_at,id`, instanceID, packageID)
	if err != nil {
		return domain.AddonPackage{}, "", nil, err
	}
	defer rows.Close()
	paths := make([]string, 0)
	for rows.Next() {
		var storagePath string
		if err := rows.Scan(&storagePath); err != nil {
			return domain.AddonPackage{}, "", nil, err
		}
		paths = append(paths, storagePath)
	}
	return pkg, path, paths, rows.Err()
}

func (s *Store) applyQQFlashPackageResult(ctx context.Context, job domain.Job) error {
	if stringFromAny(job.Result["kind"]) != "qq_flash_vpk" {
		return errors.New("QQ flash package import returned an invalid result kind")
	}
	rawPackages, ok := job.Result["packages"].([]any)
	if !ok || len(rawPackages) == 0 || len(rawPackages) > 1000 {
		return errors.New("QQ flash package import returned an invalid package list")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(rawPackages))
	for _, raw := range rawPackages {
		item, ok := raw.(map[string]any)
		if !ok {
			return errors.New("QQ flash package import returned invalid package metadata")
		}
		path := filepath.ToSlash(filepath.Clean(filepath.FromSlash(stringFromAny(item["path"]))))
		if err := validateVersionedPackageResultPath("vpk", path); err != nil {
			return err
		}
		if seen[strings.ToLower(path)] {
			return errors.New("QQ flash package import returned duplicate package paths")
		}
		seen[strings.ToLower(path)] = true
		sha := strings.ToLower(stringFromAny(item["sha256"]))
		if !validPackageSHA(sha) {
			return errors.New("QQ flash package import returned an invalid package SHA-256")
		}
		name := strings.TrimSpace(stringFromAny(item["name"]))
		if name == "" {
			name = filepath.Base(filepath.FromSlash(path))
		}
		if name != filepath.Base(filepath.FromSlash(name)) || !strings.HasSuffix(strings.ToLower(name), ".vpk") || len(name) > 255 {
			return errors.New("QQ flash package import returned an invalid package name")
		}
		versionNumber := intFromAny(item["vpk_version"])
		if versionNumber != 1 && versionNumber != 2 {
			return errors.New("QQ flash package import returned an invalid VPK version")
		}
		packageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":"+path)).String()
		_, err = tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
			VALUES(?1,?2,?3,'vpk',?4,'qq_flash','',?5,?6,true,'normal',?7,?8) ON CONFLICT(id) DO NOTHING`,
			packageID, job.InstanceID, name, fmt.Sprintf("v%d", versionNumber), sha, path, nullableUUID(job.RequestedBy), job.ID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) applyInstalledPackageScanResult(ctx context.Context, job domain.Job) error {
	if stringFromAny(job.Result["kind"]) != "installed_vpk_scan" || stringFromAny(job.Result["directory"]) != "left4dead2/addons" {
		return errors.New("installed package scan returned invalid result metadata")
	}
	rawPackages, ok := job.Result["packages"].([]any)
	if !ok || len(rawPackages) > 1000 || intFromAny(job.Result["valid_count"]) != len(rawPackages) {
		return errors.New("installed package scan returned an invalid package list")
	}
	rawRejected, ok := job.Result["rejected"].([]any)
	if !ok || len(rawPackages)+len(rawRejected) > 1000 || intFromAny(job.Result["rejected_count"]) != len(rawRejected) ||
		intFromAny(job.Result["discovered_count"]) != len(rawPackages)+len(rawRejected) {
		return errors.New("installed package scan returned invalid scan counts")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT path FROM addon_packages WHERE instance_id=?1`, job.InstanceID)
	if err != nil {
		return err
	}
	existing := make(map[string]struct{})
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		existing[strings.ToLower(filepath.ToSlash(path))] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(rawPackages))
	for _, raw := range rawPackages {
		item, ok := raw.(map[string]any)
		if !ok {
			return errors.New("installed package scan returned invalid package metadata")
		}
		path := filepath.ToSlash(filepath.Clean(filepath.FromSlash(stringFromAny(item["path"]))))
		if err := validateVersionedPackageResultPath("vpk", path); err != nil {
			return err
		}
		base := filepath.Base(filepath.FromSlash(path))
		if strings.HasPrefix(strings.ToLower(base), "l4d2panel-map-") {
			return errors.New("installed package scan returned a reserved map-library VPK")
		}
		name := strings.TrimSpace(stringFromAny(item["name"]))
		if name != base || name != filepath.Base(name) || len(name) > 255 {
			return errors.New("installed package scan returned an invalid package name")
		}
		sha := strings.ToLower(stringFromAny(item["sha256"]))
		version := intFromAny(item["vpk_version"])
		size := int64FromAny(item["size"])
		if !validPackageSHA(sha) || (version != 1 && version != 2) || size <= 0 || size > maxScannedPackageBytes {
			return errors.New("installed package scan returned invalid VPK verification metadata")
		}
		key := strings.ToLower(path)
		if _, duplicate := seen[key]; duplicate {
			return errors.New("installed package scan returned duplicate package paths")
		}
		seen[key] = struct{}{}
		if _, registered := existing[key]; registered {
			continue
		}
		packageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("installed-vpk:"+job.InstanceID+":"+key)).String()
		if _, err := tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
			VALUES(?1,?2,?3,'vpk',?4,'local_scan','',?5,?6,true,'normal',?7,?8) ON CONFLICT(id) DO NOTHING`,
			packageID, job.InstanceID, name, fmt.Sprintf("v%d", version), sha, path, nullableUUID(job.RequestedBy), job.ID); err != nil {
			return err
		}
		existing[key] = struct{}{}
	}
	return tx.Commit()
}

func (s *Store) applyInstallModResult(ctx context.Context, job domain.Job) error {
	if strings.EqualFold(stringFromAny(job.Result["kind"]), "smart") {
		return s.applySmartPluginInstallResult(ctx, job)
	}
	if kind := strings.ToLower(stringFromAny(job.Result["kind"])); kind == "sourcemod" || kind == "metamod" {
		return s.applyServerExtensionInstallResult(ctx, job, kind)
	}
	if files, ok := job.Result["files"].([]any); stringFromAny(job.Result["kind"]) == "workshop" && ok && len(files) > 0 {
		for _, raw := range files {
			file, _ := raw.(map[string]any)
			path := stringFromAny(file["path"])
			if path == "" {
				continue
			}
			version := stringFromAny(file["version"])
			if version == "" && intFromAny(file["vpk_version"]) > 0 {
				version = fmt.Sprintf("v%d", intFromAny(file["vpk_version"]))
			}
			if version == "" && len(stringFromAny(file["sha256"])) >= 12 {
				version = stringFromAny(file["sha256"])[:12]
			}
			packageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(job.ID+":"+path)).String()
			_, err := s.db.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
				VALUES(?1,?2,?3,'workshop',?4,?5,?6,?7,?8,true,?9,?10,?11) ON CONFLICT(id) DO NOTHING`, packageID,
				job.InstanceID, filepath.Base(path), version, stringFromAny(job.Payload["source"]),
				stringFromAny(file["source_id"]), stringFromAny(file["sha256"]), path, riskFromPayload(job.Payload), nullableUUID(job.RequestedBy), job.ID)
			if err != nil {
				return err
			}
		}
		return nil
	}

	name := stringFromAny(job.Payload["name"])
	if name == "" {
		name = filepath.Base(firstNonEmptyString(stringFromAny(job.Payload["source"]), stringFromAny(job.Result["source"])))
	}
	if name == "." || name == "" {
		name = firstNonEmptyString(stringFromAny(job.Result["kind"]), stringFromAny(job.Payload["kind"]), "package")
	}
	source := firstNonEmptyString(stringFromAny(job.Payload["source"]), stringFromAny(job.Result["source"]))
	kind := strings.ToLower(firstNonEmptyString(stringFromAny(job.Result["kind"]), stringFromAny(job.Payload["kind"])))
	version := firstNonEmptyString(stringFromAny(job.Result["version"]), stringFromAny(job.Result["vpk_version"]))
	if version == "" && intFromAny(job.Result["vpk_version"]) > 0 {
		version = fmt.Sprintf("v%d", intFromAny(job.Result["vpk_version"]))
	}
	sha := strings.ToLower(stringFromAny(job.Result["sha256"]))
	if version == "" && len(sha) >= 12 {
		version = sha[:12]
	}
	path := filepath.ToSlash(stringFromAny(job.Result["path"]))
	if path == "" {
		_, err := s.db.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
			VALUES(?1,?2,?3,?4,?5,?6,?7,?8,'',true,?9,?10,?1) ON CONFLICT(id) DO NOTHING`, job.ID, job.InstanceID, name,
			kind, version, source, stringFromAny(job.Result["source_id"]), sha, riskFromPayload(job.Payload), nullableUUID(job.RequestedBy))
		return err
	}
	if err := validateVersionedPackageResultPath(kind, path); err != nil {
		// Native extensions and whole SourceMod/Metamod packages are not part of
		// single-file version history; retain the existing package record path.
		if kind != "native" && kind != "sourcemod" && kind != "metamod" {
			return err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing domain.AddonPackage
	var existingLastJob string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,
		COALESCE(installed_by,''),installed_at,COALESCE(last_job_id,'')
		FROM addon_packages WHERE instance_id=?1 AND path=?2 ORDER BY installed_at DESC,id LIMIT 1`, job.InstanceID, path).Scan(
		&existing.ID, &existing.InstanceID, &existing.Name, &existing.Kind, &existing.Version, &existing.Source, &existing.SourceID,
		&existing.SHA256, &existing.Path, &existing.Enabled, &existing.Risk, &existing.InstalledBy, &existing.InstalledAt, &existingLastJob)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return lookupErr
	}
	if existingLastJob == job.ID {
		return tx.Commit()
	}
	packageID := existing.ID
	if packageID == "" {
		packageID = job.ID
	}
	if source == "" {
		source = existing.Source
	}
	if name == "" || name == "package" {
		name = firstNonEmptyString(existing.Name, filepath.Base(path))
	}

	var snapshot packageVersionMetadata
	hasSnapshot := job.Result["previous_version"] != nil
	if hasSnapshot {
		var ok bool
		snapshot, ok = parsePackageVersionMetadata(job.Result["previous_version"], path)
		if !ok {
			return errors.New("package upgrade returned invalid version metadata")
		}
	}
	if existing.ID == "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,installed_by,last_job_id)
			VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,true,?10,?11,?12)`, packageID, job.InstanceID, name, kind, version, source,
			stringFromAny(job.Result["source_id"]), sha, path, riskFromPayload(job.Payload), nullableUUID(job.RequestedBy), job.ID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE addon_packages SET name=?3,kind=?4,version=?5,source=?6,
			source_id=COALESCE(NULLIF(?7,''),source_id),sha256=?8,path=?9,enabled=true,risk=?10,
			installed_by=?11,installed_at=unixepoch('subsec')*1000000,last_job_id=?12 WHERE id=?1 AND instance_id=?2`, packageID, job.InstanceID,
			name, kind, version, source, stringFromAny(job.Result["source_id"]), sha, path, riskFromPayload(job.Payload), nullableUUID(job.RequestedBy), job.ID)
	}
	if err != nil {
		return err
	}
	if hasSnapshot {
		label := firstNonEmptyString(existing.Version, shortPackageSHA(existing.SHA256), shortPackageSHA(snapshot.SHA256))
		if err := insertPackageVersion(ctx, tx, packageID, job.InstanceID, job.ID, label, snapshot); err != nil {
			return err
		}
	}
	if err := deletePrunedPackageVersions(ctx, tx, packageID, job.Result["pruned_snapshots"]); err != nil {
		return err
	}
	if err := pruneStoredPackageVersions(ctx, tx, packageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) applyServerExtensionInstallResult(ctx context.Context, job domain.Job, kind string) error {
	rawFiles, ok := job.Result["managed_files"].([]any)
	if !ok || len(rawFiles) == 0 || len(rawFiles) > 50000 || !validPackageSHA(strings.ToLower(stringFromAny(job.Result["sha256"]))) {
		return errors.New("server extension installation returned an invalid managed file list")
	}
	type managedFile struct {
		path, sha string
		size      int64
	}
	files := make([]managedFile, 0, len(rawFiles))
	seen := make(map[string]bool, len(rawFiles))
	for _, raw := range rawFiles {
		item, ok := raw.(map[string]any)
		path := filepath.ToSlash(stringFromAny(item["path"]))
		sha := strings.ToLower(stringFromAny(item["sha256"]))
		size := int64FromAny(item["size"])
		key := strings.ToLower(path)
		if !ok || validateServerExtensionManagedPath(kind, path) != nil || !validPackageSHA(sha) || size < 0 || seen[key] {
			return errors.New("server extension installation returned an invalid managed file")
		}
		seen[key] = true
		files = append(files, managedFile{path: path, sha: sha, size: size})
	}
	packageID := strings.TrimSpace(stringFromAny(job.Result["package_id"]))
	if packageID == "" {
		packageID = strings.TrimSpace(stringFromAny(job.Payload["package_id"]))
	}
	if packageID != "" && uuid.Validate(packageID) != nil {
		return errors.New("server extension installation returned an invalid package id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if packageID == "" {
		lookupErr := tx.QueryRowContext(ctx, `SELECT id FROM addon_packages WHERE instance_id=?1 AND lower(kind)=?2
			ORDER BY installed_at DESC,id DESC LIMIT 1`, job.InstanceID, kind).Scan(&packageID)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
	}
	if packageID == "" {
		packageID = job.ID
	}
	var existingInstance, existingKind string
	lookupErr := tx.QueryRowContext(ctx, `SELECT instance_id,kind FROM addon_packages WHERE id=?1`, packageID).Scan(&existingInstance, &existingKind)
	if lookupErr == nil && (existingInstance != job.InstanceID || !strings.EqualFold(existingKind, kind)) {
		return errors.New("server extension update targets a different or incompatible package")
	}
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return lookupErr
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM addon_packages WHERE instance_id=?1 AND lower(kind)=?2 AND id<>?3`, job.InstanceID, kind, packageID); err != nil {
		return err
	}
	name := "SourceMod"
	if kind == "metamod" {
		name = "Metamod:Source"
	}
	version := strings.TrimSpace(stringFromAny(job.Result["version"]))
	sha := strings.ToLower(stringFromAny(job.Result["sha256"]))
	source := firstNonEmptyString(stringFromAny(job.Result["source"]), stringFromAny(job.Payload["source"]))
	risk := riskFromPayload(job.Payload)
	_, err = tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,version,source,sha256,path,enabled,risk,installed_by,last_job_id,file_count,restart_required)
		VALUES(?1,?2,?3,?4,?5,?6,?7,'',true,?8,?9,?10,?11,false)
		ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,kind=EXCLUDED.kind,version=EXCLUDED.version,source=EXCLUDED.source,
		sha256=EXCLUDED.sha256,path='',enabled=true,risk=EXCLUDED.risk,installed_by=EXCLUDED.installed_by,
		installed_at=unixepoch('subsec')*1000000,last_job_id=EXCLUDED.last_job_id,file_count=EXCLUDED.file_count,restart_required=false
		WHERE addon_packages.instance_id=EXCLUDED.instance_id AND lower(addon_packages.kind)=lower(EXCLUDED.kind)`, packageID, job.InstanceID,
		name, kind, version, source, sha, risk, nullableUUID(job.RequestedBy), job.ID, len(files))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM addon_package_files WHERE package_id=?1 AND instance_id=?2`, packageID, job.InstanceID); err != nil {
		return err
	}
	for _, file := range files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint)
			VALUES(?1,?2,?3,?4,?5,true)`, packageID, job.InstanceID, file.path, file.sha, file.size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validateServerExtensionManagedPath(kind, value string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(value))))
	if cleaned != value || filepath.IsAbs(filepath.FromSlash(cleaned)) || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, `\`) {
		return errors.New("invalid server extension path")
	}
	lower := strings.ToLower(cleaned)
	if kind == "metamod" && (lower == "left4dead2/addons/metamod.vdf" || strings.HasPrefix(lower, "left4dead2/addons/metamod/")) {
		return nil
	}
	if kind == "sourcemod" && (strings.HasPrefix(lower, "left4dead2/cfg/sourcemod/") ||
		(strings.HasPrefix(lower, "left4dead2/addons/sourcemod/") && !strings.EqualFold(lower, "left4dead2/addons/sourcemod/configs/admins_simple.ini"))) {
		return nil
	}
	return errors.New("server extension path is outside its fixed component directory")
}

func (s *Store) applySmartPluginInstallResult(ctx context.Context, job domain.Job) error {
	rawPackages, ok := job.Result["packages"].([]any)
	if !ok || len(rawPackages) == 0 || len(rawPackages) > 256 {
		return errors.New("smart plugin installation returned an invalid package list")
	}
	restartRequired, _ := job.Result["restart_required"].(bool)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seenPackages := make(map[string]bool, len(rawPackages))
	seenPaths := make(map[string]bool)
	for _, raw := range rawPackages {
		item, ok := raw.(map[string]any)
		if !ok {
			return errors.New("smart plugin installation returned invalid package metadata")
		}
		packageID := strings.TrimSpace(stringFromAny(item["package_id"]))
		name := strings.TrimSpace(stringFromAny(item["name"]))
		sha := strings.ToLower(stringFromAny(item["sha256"]))
		files, ok := item["files"].([]any)
		if uuid.Validate(packageID) != nil || seenPackages[packageID] || name == "" || len(name) > 255 || !validPackageSHA(sha) || !ok || len(files) == 0 || len(files) > 5000 {
			return errors.New("smart plugin installation returned invalid package metadata")
		}
		seenPackages[packageID] = true
		var existingInstance, existingKind string
		lookupErr := tx.QueryRowContext(ctx, `SELECT instance_id,kind FROM addon_packages WHERE id=?1`, packageID).Scan(&existingInstance, &existingKind)
		if lookupErr == nil && (existingInstance != job.InstanceID || !strings.EqualFold(existingKind, "smart")) {
			return errors.New("smart plugin update targets a different or incompatible package")
		}
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		nativeRisk, _ := item["native_risk"].(bool)
		risk := "normal"
		if nativeRisk {
			risk = "confirmed_native"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO addon_packages(id,instance_id,name,kind,source,sha256,path,enabled,risk,installed_by,last_job_id,file_count,restart_required)
			VALUES(?1,?2,?3,'smart',?4,?5,'',true,?6,?7,?8,?9,?10)
			ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,source=EXCLUDED.source,sha256=EXCLUDED.sha256,enabled=true,risk=EXCLUDED.risk,
			installed_by=EXCLUDED.installed_by,installed_at=unixepoch('subsec')*1000000,last_job_id=EXCLUDED.last_job_id,file_count=EXCLUDED.file_count,restart_required=EXCLUDED.restart_required`,
			packageID, job.InstanceID, name, stringFromAny(job.Payload["name"]), sha, risk, nullableUUID(job.RequestedBy), job.ID, len(files), restartRequired)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM addon_package_files WHERE package_id=?1 AND instance_id=?2`, packageID, job.InstanceID); err != nil {
			return err
		}
		for _, rawFile := range files {
			file, ok := rawFile.(map[string]any)
			path := filepath.ToSlash(stringFromAny(file["path"]))
			fileSHA := strings.ToLower(stringFromAny(file["sha256"]))
			size := int64FromAny(file["size"])
			entrypoint, _ := file["entrypoint"].(bool)
			if !ok || validateSmartPluginResultPath(path) != nil || !validPackageSHA(fileSHA) || size < 0 || seenPaths[strings.ToLower(path)] {
				return errors.New("smart plugin installation returned an invalid or duplicate file")
			}
			seenPaths[strings.ToLower(path)] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO addon_package_files(package_id,instance_id,path,sha256,size,entrypoint)
				VALUES(?1,?2,?3,?4,?5,?6)`, packageID, job.InstanceID, path, fileSHA, size, entrypoint); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func validateSmartPluginResultPath(value string) error {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(value))))
	if cleaned != value || filepath.IsAbs(filepath.FromSlash(cleaned)) || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, `\`) {
		return errors.New("invalid smart plugin path")
	}
	lower := strings.ToLower(cleaned)
	if !strings.HasPrefix(lower, "left4dead2/addons/") && !strings.HasPrefix(lower, "left4dead2/cfg/") && !strings.HasPrefix(lower, "left4dead2/modes/") && !strings.HasPrefix(lower, "left4dead2/scripts/vscripts/") {
		return errors.New("smart plugin path is outside approved directories")
	}
	switch strings.ToLower(filepath.Ext(cleaned)) {
	case ".smx", ".sp", ".inc", ".txt", ".cfg", ".nut", ".dll", ".so", ".vdf", ".autoload":
		return nil
	default:
		return errors.New("smart plugin file type is unsupported")
	}
}

func (s *Store) applySmartPluginFileEditResult(ctx context.Context, job domain.Job) error {
	packageID := strings.TrimSpace(stringFromAny(job.Payload["package_id"]))
	path := filepath.ToSlash(stringFromAny(job.Result["path"]))
	expected := strings.ToLower(stringFromAny(job.Payload["expected_sha256"]))
	previous := strings.ToLower(stringFromAny(job.Result["previous_sha256"]))
	sha := strings.ToLower(stringFromAny(job.Result["sha256"]))
	aggregate := strings.ToLower(stringFromAny(job.Result["aggregate_sha256"]))
	size := int64FromAny(job.Result["size"])
	if uuid.Validate(packageID) != nil || packageID != stringFromAny(job.Result["package_id"]) || path != stringFromAny(job.Payload["relative_path"]) ||
		validateSmartPluginResultPath(path) != nil || !smartPluginFileEditable("smart", path, false) || !validPackageSHA(expected) ||
		previous != expected || !validPackageSHA(sha) || !validPackageSHA(aggregate) || size < 0 || size > 8<<20 {
		return errors.New("smart plugin file edit returned invalid metadata")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentSHA string
	var entrypoint bool
	err = tx.QueryRowContext(ctx, `SELECT sha256,entrypoint FROM addon_package_files
		WHERE package_id=?1 AND instance_id=?2 AND path=?3`, packageID, job.InstanceID, path).Scan(&currentSHA, &entrypoint)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if entrypoint || (!strings.EqualFold(currentSHA, expected) && !strings.EqualFold(currentSHA, sha)) {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE addon_package_files SET sha256=?4,size=?5
		WHERE package_id=?1 AND instance_id=?2 AND path=?3`, packageID, job.InstanceID, path, sha, size); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE addon_packages SET sha256=?3,last_job_id=?4
		WHERE id=?1 AND instance_id=?2 AND lower(kind)='smart'`, packageID, job.InstanceID, aggregate, job.ID)
	if err != nil {
		return err
	}
	if resultRowsAffected(result) == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) applyPackageRollbackResult(ctx context.Context, job domain.Job) error {
	packageID := stringFromAny(job.Payload["package_id"])
	versionID := stringFromAny(job.Payload["version_id"])
	if packageID == "" || versionID == "" {
		return errors.New("package rollback result is missing version identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current domain.AddonPackage
	var lastJob string
	err = tx.QueryRowContext(ctx, `SELECT id,instance_id,name,kind,version,source,source_id,sha256,path,enabled,risk,
		COALESCE(installed_by,''),installed_at,COALESCE(last_job_id,'')
		FROM addon_packages WHERE id=?1 AND instance_id=?2`, packageID, job.InstanceID).Scan(
		&current.ID, &current.InstanceID, &current.Name, &current.Kind, &current.Version, &current.Source, &current.SourceID,
		&current.SHA256, &current.Path, &current.Enabled, &current.Risk, &current.InstalledBy, &current.InstalledAt, &lastJob)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if lastJob == job.ID {
		return tx.Commit()
	}
	path := filepath.ToSlash(stringFromAny(job.Result["path"]))
	sha := strings.ToLower(stringFromAny(job.Result["sha256"]))
	if path != current.Path || sha != strings.ToLower(stringFromAny(job.Payload["expected_sha256"])) {
		return errors.New("package rollback result does not match the requested target")
	}
	if err := validateVersionedPackageResultPath(current.Kind, path); err != nil {
		return err
	}
	snapshot, ok := parsePackageVersionMetadata(job.Result["previous_version"], path)
	if !ok {
		return errors.New("package rollback did not preserve the displaced version")
	}
	label := firstNonEmptyString(current.Version, shortPackageSHA(current.SHA256), shortPackageSHA(snapshot.SHA256))
	if err := insertPackageVersion(ctx, tx, packageID, job.InstanceID, job.ID, label, snapshot); err != nil {
		return err
	}
	version := firstNonEmptyString(stringFromAny(job.Result["version"]), stringFromAny(job.Payload["restore_version"]), shortPackageSHA(sha))
	if _, err := tx.ExecContext(ctx, `UPDATE addon_packages SET version=?3,sha256=?4,path=?5,enabled=true,
		installed_by=?6,installed_at=unixepoch('subsec')*1000000,last_job_id=?7 WHERE id=?1 AND instance_id=?2`, packageID, job.InstanceID,
		version, sha, path, nullableUUID(job.RequestedBy), job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM addon_package_versions WHERE id=?1 AND package_id=?2 AND instance_id=?3`, versionID, packageID, job.InstanceID); err != nil {
		return err
	}
	if err := deletePrunedPackageVersions(ctx, tx, packageID, job.Result["pruned_snapshots"]); err != nil {
		return err
	}
	if err := pruneStoredPackageVersions(ctx, tx, packageID); err != nil {
		return err
	}
	return tx.Commit()
}

func insertPackageVersion(ctx context.Context, tx *sql.Tx, packageID, instanceID, sourceJobID, version string, metadata packageVersionMetadata) error {
	manifest, _ := json.Marshal(map[string]any{"files": []any{map[string]any{
		"path": metadata.TargetPath, "sha256": metadata.SHA256, "size": metadata.Size,
	}}})
	_, err := tx.ExecContext(ctx, `INSERT INTO addon_package_versions(id,package_id,instance_id,source_job_id,version,sha256,size,storage_path,target_path,manifest,created_at)
		VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11) ON CONFLICT(source_job_id,storage_path) DO NOTHING`, uuid.NewString(), packageID,
		instanceID, sourceJobID, version, metadata.SHA256, metadata.Size, metadata.StoragePath, metadata.TargetPath, string(manifest), metadata.CreatedAt)
	return err
}

func parsePackageVersionMetadata(value any, expectedTarget string) (packageVersionMetadata, bool) {
	raw, ok := value.(map[string]any)
	if !ok {
		return packageVersionMetadata{}, false
	}
	metadata := packageVersionMetadata{
		StoragePath: filepath.ToSlash(stringFromAny(raw["storage_path"])),
		TargetPath:  filepath.ToSlash(stringFromAny(raw["target_path"])),
		SHA256:      strings.ToLower(stringFromAny(raw["sha256"])),
		Size:        int64FromAny(raw["size"]),
		CreatedAt:   time.Now().UTC(),
	}
	if parsed, err := time.Parse(time.RFC3339Nano, stringFromAny(raw["created_at"])); err == nil {
		metadata.CreatedAt = parsed
	}
	if metadata.TargetPath != expectedTarget || metadata.Size < 0 || !validPackageSHA(metadata.SHA256) {
		return packageVersionMetadata{}, false
	}
	cleanStorage := filepath.ToSlash(filepath.Clean(filepath.FromSlash(metadata.StoragePath)))
	if cleanStorage != metadata.StoragePath || strings.Contains(cleanStorage, `\`) || strings.HasPrefix(cleanStorage, "/") || !strings.HasPrefix(cleanStorage, "package-versions/") {
		return packageVersionMetadata{}, false
	}
	segments := strings.Split(cleanStorage, "/")
	if len(segments) != 4 || len(segments[1]) != 64 || len(segments[2]) != 64 || len(segments[3]) != 68 || !strings.HasSuffix(segments[3], ".bin") {
		return packageVersionMetadata{}, false
	}
	for _, encoded := range []string{segments[1], segments[2], strings.TrimSuffix(segments[3], ".bin")} {
		if _, err := hex.DecodeString(encoded); err != nil {
			return packageVersionMetadata{}, false
		}
	}
	metadata.StoragePath = cleanStorage
	return metadata, true
}

func validateVersionedPackageResultPath(kind, path string) error {
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	base := filepath.Base(filepath.FromSlash(path))
	kind = strings.ToLower(kind)
	switch {
	case kind == "vpk" || kind == "vpk_url" || kind == "workshop":
		if path != filepath.ToSlash(filepath.Join("left4dead2", "addons", base)) || !strings.HasSuffix(strings.ToLower(base), ".vpk") {
			return errors.New("VPK package path is outside the approved addons directory")
		}
	case kind == "plugin" || kind == "smx":
		if path != filepath.ToSlash(filepath.Join("left4dead2", "addons", "sourcemod", "plugins", base)) || !strings.HasSuffix(strings.ToLower(base), ".smx") {
			return errors.New("SourceMod package path is outside the approved plugins directory")
		}
	default:
		return errors.New("version history supports only VPK and SourceMod SMX packages")
	}
	return nil
}

func deletePrunedPackageVersions(ctx context.Context, tx *sql.Tx, packageID string, raw any) error {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		path := filepath.ToSlash(stringFromAny(value))
		if !strings.HasPrefix(path, "package-versions/") || strings.Contains(path, "..") {
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil
	}
	rawPaths, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM addon_package_versions WHERE package_id=?1
		AND storage_path IN (SELECT value FROM json_each(?2))`, packageID, string(rawPaths))
	return err
}

func pruneStoredPackageVersions(ctx context.Context, tx *sql.Tx, packageID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM addon_package_versions WHERE id IN (
		SELECT id FROM addon_package_versions WHERE package_id=?1 ORDER BY created_at DESC,id DESC OFFSET 3
	)`, packageID)
	return err
}

func validPackageSHA(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func shortPackageSHA(value string) string {
	if len(value) >= 12 {
		return strings.ToLower(value[:12])
	}
	return ""
}
