import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { StoreProvider } from "./runtime/context";
import { SimStore } from "./store/simstore";
import { bindControl } from "./store/simstore";
import { installOverlay } from "./runtime/overlay";
import { bindStory } from "./runtime/story";
import { loadStory } from "./scenarios";
import "./styles/tokens.css";
import "./styles/slack.css";
import "./styles/overlay.css";

// Entry point. The capture engine loads this page with:
//   ?scenario=<name>&theme=<light|dark>&scene=<id>
// builds the named scenario, and then drives the conversation through
// window.__showcase (bound below). Reading the URL keeps captures reproducible.

const params = new URLSearchParams(window.location.search);
const story = loadStory(params.get("scenario"));
const theme = params.get("theme") === "light" ? "light" : "dark";
document.documentElement.dataset.theme = theme;

const store = new SimStore(story.scenario);
const control = installOverlay();
bindControl(store, control);
bindStory(story, window.__showcase!);

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <StoreProvider store={store}>
      <App initialTheme={theme} />
    </StoreProvider>
  </StrictMode>,
);
