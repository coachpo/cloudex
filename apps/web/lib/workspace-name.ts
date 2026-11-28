const adjectives = [
  "amber", "autumn", "bright", "brisk", "calm", "clear", "cloudy", "cool",
  "coral", "crisp", "dusky", "early", "faint", "fresh", "gentle", "golden",
  "green", "hidden", "high", "indigo", "light", "little", "lively", "lunar",
  "misty", "mossy", "quiet", "rapid", "rosy", "silver", "smooth", "soft",
  "solar", "steady", "still", "stone", "sunny", "swift", "warm", "windy",
];

const nouns = [
  "alder", "aspen", "bay", "birch", "brook", "cedar", "clover", "comet",
  "cove", "crane", "creek", "dawn", "dune", "elm", "falcon", "fern",
  "finch", "fjord", "grove", "harbor", "hazel", "hill", "island", "lark",
  "laurel", "leaf", "maple", "marsh", "meadow", "moon", "orchid", "otter",
  "pebble", "pine", "reed", "ridge", "river", "sparrow", "willow", "wren",
];

export function randomWorkspaceName(previous?: string) {
  const adjective = adjectives[Math.floor(Math.random() * adjectives.length)];
  const nounIndex = Math.floor(Math.random() * nouns.length);
  const name = `${adjective}-${nouns[nounIndex]}`;
  return name === previous ? `${adjective}-${nouns[(nounIndex + 1) % nouns.length]}` : name;
}
