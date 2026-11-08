import "../ds/styles.css";
import { overwriteGetLocale } from "../paraglide/runtime.js";

// Tests read English; the French catalogue is checked for completeness by knip
// and the i18n compile, not by assertions.
overwriteGetLocale(() => "en");
