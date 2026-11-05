# Notes

Everything is build with AI using pi and some claude. I just review code and write specs. I assume that the server time is UTC and all events are same timezone/coming converted to the server's time zone. Major tradeoff is due to the inputs live in memory (heap). This way decisions never hit I/O, so performance is on point - it directly hit the heap which is O(1). If I had another day I would think about what to do if database gets down.
