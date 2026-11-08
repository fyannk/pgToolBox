import type { ReactNode } from "react";
import { render } from "vitest-browser-react";
import { StoryFrame } from "../../.storybook/StoryFrame";

/** Renders inside the same providers as the stories: no retries, fixture clock, memory router. */
export function renderInFrame(ui: ReactNode) {
  return render(<StoryFrame>{() => ui}</StoryFrame>);
}
