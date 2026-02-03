//! Pre-computed test content for validation testing.
//!
//! Contains Unicode sequences, edge cases, and special characters for
//! verifying daemon response handling correctness.

/// Unicode test content containing various character classes.
pub struct UnicodeTestData {
    /// Emoji (2-4 byte sequences)
    pub emoji: &'static [&'static str],
    /// CJK characters
    pub cjk: &'static [&'static str],
    /// 4-byte UTF-8 characters (supplementary planes)
    pub four_byte: &'static [&'static str],
    /// Mixed content combining different character types
    pub mixed: &'static [&'static str],
}

/// Pre-computed Unicode test data.
pub const UNICODE_DATA: UnicodeTestData = UnicodeTestData {
    emoji: &[
        "\u{1F600}", // 😀 Grinning Face
        "\u{1F4A1}", // 💡 Light Bulb
        "\u{2764}",  // ❤ Red Heart (actually 3-byte)
        "\u{1F680}", // 🚀 Rocket
        "\u{1F389}", // 🎉 Party Popper
        "\u{1F44D}", // 👍 Thumbs Up
        "\u{2728}",  // ✨ Sparkles
        "\u{1F525}", // 🔥 Fire
        "\u{1F31F}", // 🌟 Glowing Star
        "\u{1F3AF}", // 🎯 Direct Hit
    ],
    cjk: &[
        "\u{4E2D}", // 中
        "\u{6587}", // 文
        "\u{5B57}", // 字
        "\u{7B26}", // 符
        "\u{6D4B}", // 测
        "\u{8BD5}", // 试
        "\u{65E5}", // 日
        "\u{672C}", // 本
        "\u{8A9E}", // 語
        "\u{97D3}", // 韓
    ],
    four_byte: &[
        "\u{1D400}", // 𝐀 Mathematical Bold Capital A
        "\u{1D401}", // 𝐁 Mathematical Bold Capital B
        "\u{1D402}", // 𝐂 Mathematical Bold Capital C
        "\u{10348}", // 𐍈 Gothic Letter Hwair
        "\u{1F600}", // 😀 Grinning Face (emoji are 4-byte too)
        "\u{1F4BB}", // 💻 Laptop
        "\u{1F9D1}", // 🧑 Person
        "\u{1F308}", // 🌈 Rainbow
    ],
    mixed: &[
        "Hello 世界! 🌍",
        "Test 测试 🧪",
        "Code 代码 💻",
        "Math: 𝐀𝐁𝐂",
        "日本語テスト 🎌",
        "한국어 테스트 🇰🇷",
        "Emoji mix: 🚀✨🔥",
    ],
};

/// Returns an iterator over all Unicode content chunks for streaming.
pub fn unicode_chunks() -> impl Iterator<Item = &'static str> {
    UNICODE_DATA
        .emoji
        .iter()
        .chain(UNICODE_DATA.cjk.iter())
        .chain(UNICODE_DATA.four_byte.iter())
        .chain(UNICODE_DATA.mixed.iter())
        .copied()
}

/// Short chunks for testing minimal chunk handling (1-2 bytes each).
pub const SHORT_CHUNKS: &[&str] = &[
    "H", "e", "l", "l", "o", " ", "w", "o", "r", "l", "d", "!", " ", "T", "h", "i", "s", " ", "i",
    "s", " ", "a", " ", "t", "e", "s", "t", ".", " ", "E", "a", "c", "h", " ", "c", "h", "u", "n",
    "k", " ", "i", "s", " ", "s", "m", "a", "l", "l", ".",
];

/// Long chunk content (4KB target).
/// This is a single long string that will be split into 4KB chunks.
pub fn long_chunk_content() -> String {
    // Generate ~4KB of content per chunk
    const CHUNK_SIZE: usize = 4096;
    let base = "This is a long chunk of text for testing large SSE payload handling. \
                The streaming daemon should correctly handle chunks of varying sizes, \
                including large chunks that may require multiple TCP segments. \
                This content is repeated to reach the target chunk size. ";
    let mut content = String::with_capacity(CHUNK_SIZE);
    while content.len() < CHUNK_SIZE {
        content.push_str(base);
    }
    content.truncate(CHUNK_SIZE);
    content
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_unicode_emoji_are_valid_utf8() {
        for emoji in UNICODE_DATA.emoji {
            assert!(emoji.is_ascii() || emoji.len() > 1);
            // Verify it's valid UTF-8 (Rust strings are always valid UTF-8)
            assert!(!emoji.is_empty());
        }
    }

    #[test]
    fn test_unicode_cjk_are_valid_utf8() {
        for cjk in UNICODE_DATA.cjk {
            // CJK characters are 3 bytes in UTF-8
            assert!(cjk.len() >= 3);
        }
    }

    #[test]
    fn test_unicode_four_byte_are_valid_utf8() {
        for ch in UNICODE_DATA.four_byte {
            // Some may be emoji (4 bytes), some mathematical symbols
            assert!(ch.len() >= 3);
        }
    }

    #[test]
    fn test_unicode_chunks_iterator() {
        let chunks: Vec<_> = unicode_chunks().collect();
        assert!(!chunks.is_empty());
        // Should have all categories
        let total = UNICODE_DATA.emoji.len()
            + UNICODE_DATA.cjk.len()
            + UNICODE_DATA.four_byte.len()
            + UNICODE_DATA.mixed.len();
        assert_eq!(chunks.len(), total);
    }

    #[test]
    fn test_short_chunks_are_small() {
        for chunk in SHORT_CHUNKS {
            assert!(chunk.len() <= 2);
        }
    }

    #[test]
    fn test_long_chunk_content_size() {
        let content = long_chunk_content();
        assert!(content.len() >= 4000); // At least 4KB
    }
}
