from __future__ import annotations

import logging
import re
import shutil
from pathlib import Path

log = logging.getLogger(__name__)

_SENTINEL_ENUM        = "// >>> generated-filters-enum <<<"
_SENTINEL_LABEL       = "// >>> generated-filters-label <<<"
_SENTINEL_ICON        = "// >>> generated-filters-icon <<<"
_SENTINEL_NEEDS_TIME  = "// >>> generated-filters-needstime <<<"
_SENTINEL_ASSET       = "// >>> generated-filters-shaderasset <<<"


class MirrorBoothIntegrator:
    """Idempotent installer of generated .frag shaders into the MirrorBooth Flutter app.

    Mirrors the sentinel-anchor approach from MirrorBooth/pipeline/integrator.py.
    Every sentinel anchor in mirror_filter.dart receives one insertion per shader.
    Re-running with the same name is a no-op.
    """

    def __init__(self, mirrorbooth_root: Path) -> None:
        self.shaders_dir   = mirrorbooth_root / "shaders"
        self.filter_dart   = mirrorbooth_root / "lib" / "core" / "mirror_filter.dart"
        self.pubspec_yaml  = mirrorbooth_root / "pubspec.yaml"

        for path in (self.shaders_dir, self.filter_dart, self.pubspec_yaml):
            if not path.exists():
                raise FileNotFoundError(f"MirrorBooth path not found: {path}")

    # ------------------------------------------------------------------ public

    def install(
        self,
        *,
        name: str,
        frag_content: bytes,
        needs_time: bool,
        label: str,
        icon: str,
    ) -> bool:
        """Install shader into MirrorBooth. Returns False if already installed (idempotent)."""
        enum_name = _to_enum_name(name)
        asset_path = f"shaders/{name}.frag"

        if self._already_installed(enum_name):
            log.info("integrator: %s already installed — skipping", enum_name)
            return False

        self._copy_shader(name, frag_content)
        self._register_pubspec(asset_path)
        self._register_filter_dart(enum_name, label, icon, needs_time, asset_path)
        log.info("integrator: installed %s (%s)", name, enum_name)
        return True

    # ----------------------------------------------------------------- private

    def _already_installed(self, enum_name: str) -> bool:
        dart = self.filter_dart.read_text()
        return f"  {enum_name}," in dart

    def _copy_shader(self, name: str, content: bytes) -> None:
        dest = self.shaders_dir / f"{name}.frag"
        dest.write_bytes(content)
        log.debug("integrator: wrote %s", dest)

    def _register_pubspec(self, asset_path: str) -> None:
        text = self.pubspec_yaml.read_text()
        entry = f"    - {asset_path}"
        if entry in text:
            return
        text = text.replace("  shaders:\n", f"  shaders:\n{entry}\n", 1)
        self.pubspec_yaml.write_text(text)

    def _register_filter_dart(
        self,
        enum_name: str,
        label: str,
        icon: str,
        needs_time: bool,
        asset_path: str,
    ) -> None:
        dart = self.filter_dart.read_text()

        dart = _insert_after_sentinel(dart, _SENTINEL_ENUM,       f"  {enum_name},")
        dart = _insert_after_sentinel(dart, _SENTINEL_LABEL,      f"      MirrorFilter.{enum_name} => '{label}',")
        dart = _insert_after_sentinel(dart, _SENTINEL_ICON,       f"      MirrorFilter.{enum_name} => '{icon}',")
        dart = _insert_after_sentinel(dart, _SENTINEL_ASSET,      f"      MirrorFilter.{enum_name} => '{asset_path}',")

        if needs_time:
            dart = _insert_after_sentinel(
                dart, _SENTINEL_NEEDS_TIME, f"      MirrorFilter.{enum_name} => true,"
            )

        self.filter_dart.write_text(dart)


def _insert_after_sentinel(text: str, sentinel: str, line: str) -> str:
    if sentinel not in text:
        raise ValueError(f"Sentinel not found in mirror_filter.dart: {sentinel!r}")
    if line in text:
        return text  # idempotent
    return text.replace(sentinel, f"{sentinel}\n{line}", 1)


def _to_enum_name(name: str) -> str:
    """Convert 'filter_neon_bokeh' → 'neonBokeh' (lowerCamelCase, strip 'filter_' prefix)."""
    parts = re.sub(r"^filter_", "", name).split("_")
    return parts[0] + "".join(p.capitalize() for p in parts[1:])
