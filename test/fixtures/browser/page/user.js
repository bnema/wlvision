// Prefs that keep a first run quiet and its profile small, because a session's
// home is a bounded tmpfs and its window is all the test looks at.
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.startup.page", 0);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.cache.disk.enable", false);
user_pref("browser.cache.memory.capacity", 8192);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("toolkit.telemetry.enabled", false);
user_pref("app.update.enabled", false);
user_pref("extensions.update.enabled", false);
user_pref("layers.acceleration.disabled", true);
user_pref("gfx.canvas.accelerated", false);
user_pref("media.hardware-video-decoding.enabled", false);
user_pref("dom.ipc.processCount", 1);
