/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package compare is the statistics behind an evaluation experiment: it says,
// from the results of running two or more versions of an agent over the same
// test cases, whether one is better than another, and how sure that is. It is
// pure. It reads rows shaped like the eval_results and eval_grades tables and
// returns numbers, with no database and no cluster, so every figure can be
// reproduced from its input.
//
// # What is compared
//
// Every arm runs the same cases, so the comparison is paired: case 7 under the
// baseline is set against case 7 under the candidate. A case counts as
// improved when the baseline fails it and the candidate passes, regressed when
// it is the other way round, and tied when both did the same. Cases that did
// not finish or are not yet graded are skipped and listed, never hidden.
//
// # The methods, in plain language
//
// Pass rate for one arm. The rate is shown with a Wilson score interval: the
// range of true pass rates that the observed result is consistent with. Ten
// passes in ten does not mean the agent always passes, and the interval says
// how far below 100% it could really be.
//
// McNemar's test. Only the cases where the two arms disagree tell them apart.
// If the candidate wins 9 of 10 disagreements, that is unlikely by luck; if it
// wins 5 of 10 it is a coin toss. The p-value is how often a split at least
// that lopsided would turn up if the arms were equally good. Up to 100
// disagreements it is computed exactly; beyond that by the usual chi-square
// approximation with continuity correction.
//
// Difference in pass rate. The candidate's rate minus the baseline's, with a
// confidence interval from Newcombe's method for paired proportions. A 95%
// interval means: if the experiment were repeated many times, about 95 of 100
// such intervals would contain the true difference.
//
// Scores, cost and latency. For each case the difference between the arms is
// taken, and the mean difference gets a bootstrap interval: the cases are
// resampled with replacement 2000 times and the middle 95% of the resulting
// means is reported. The resampling uses a fixed seed, normally the experiment
// ID, so the same experiment always shows the same interval. Cost and latency
// are skewed by a few slow or dear runs, so the median difference and its
// change relative to the baseline's median are given too.
//
// # What the verdict means
//
// The verdict is one of better, worse, no clear difference, or not enough
// data. It is decided on pass rate only.
//
//   - Not enough data: fewer than 30 paired cases. With so few cases nothing
//     is said, whatever the numbers look like.
//   - Better or worse: the interval for the difference lies entirely on one
//     side of zero and McNemar's test agrees at the confidence level. Both
//     must hold; where they differ at the edge the verdict stays cautious.
//   - No clear difference: anything else. This does not mean the agents are
//     the same. It means this experiment cannot tell them apart; a bigger
//     dataset might.
//
// Non-inferiority is reported next to the verdict, not as a fifth verdict. It
// is true when there were enough cases and the lowest plausible difference is
// above minus the margin (default 5 percentage points): the candidate is at
// most that much worse, which is what "the cheaper model is no worse" means
// precisely. It is deliberately one-sided and does not claim the two are equal.
//
// # Choices made where the proposal is open
//
// These are the most cautious readings; each is a place a reader may want to
// differ.
//
//   - A run that failed or timed out is a failed case, not a skipped one.
//   - A succeeded run passes when every required grader passes and more than
//     half of the optional graders, by weight, pass. A result with any grade
//     that is not "ok" is unusable until that is resolved.
//   - With repeats, a case passes when more than half of its usable repeats
//     pass; a tie fails. Scores, cost and latency are averaged per case first.
//     The share of cases whose repeats disagree is reported as stability.
//   - Better and worse need both the interval and the test; the margin does not
//     move either threshold.
//   - The bootstrap uses 2000 resamples, not the 1000 the proposal mentions,
//     and with fewer than 8 pairs it reports the range of the observed
//     differences instead, flagged as approximate, because a bootstrap of a
//     handful of values is far too narrow.
//
// # Limits
//
// Intervals describe sampling luck in the cases chosen, not whether those cases
// resemble real traffic. Many comparisons at once raise the chance that one
// looks significant by accident; no correction is applied, so read an
// experiment with many arms with that in mind. A flaky agent with few repeats
// can look better or worse than it is. An LLM judge's errors are not part of
// these intervals.
//
// The Wilson interval, the normal quantile, the seeded bootstrap and the
// non-inferiority comparisons are shared with internal/routingbench.
package compare
